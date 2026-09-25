use super::*;

/// These tests are about files rather than about a page, so they run at a RAM
/// session's 4 KiB over ordinary memfds, which needs no HugeTLB pool. The Go
/// suite in internal/vmtest maps read-only HugeTLB files through the example
/// client.
const PAGE: usize = MIN_PAGE_SIZE;

fn spec(pages: usize) -> MemoryRegionSpec {
    MemoryRegionSpec {
        kind: MemoryRegionKind::Ram,
        len: pages * PAGE,
    }
}

/// Serves a session of this many pages whose private file has as many.
fn serve_files(pages: usize, script: impl FnOnce(&mut UnixStream) + Send) -> io::Result<()> {
    let private = named_file(c"sproutfs-files-private", PAGE, pages);
    serve_backing(
        spec(pages),
        attachment_for(PAGE),
        file_for(wire::PRIVATE_FILE, PAGE, pages * PAGE),
        &private,
        script,
    )
}

/// The mappings of the file named name in this process, in address order: the
/// offset in the file each maps from, and whether it is a private ('p') or a
/// shared ('s') mapping.
fn mappings_of(name: &str) -> Vec<(u64, char)> {
    let path = format!("/memfd:{name} ");
    std::fs::read_to_string("/proc/self/maps")
        .unwrap()
        .lines()
        .filter(|line| line.contains(&path))
        .map(|line| {
            let mut fields = line.split_whitespace();
            let (_range, perms) = (fields.next().unwrap(), fields.next().unwrap());
            let offset = u64::from_str_radix(fields.next().unwrap(), 16).unwrap();
            (offset, perms.chars().nth(3).unwrap())
        })
        .collect()
}

/// How many descriptors this process holds of the file named name.
fn descriptors_of(name: &str) -> usize {
    let want = format!("/memfd:{name} (deleted)");
    std::fs::read_dir("/proc/self/fd")
        .unwrap()
        .filter_map(|entry| std::fs::read_link(entry.unwrap().path()).ok())
        .filter(|target| target.to_str() == Some(want.as_str()))
        .count()
}

/// A MAP of one page of file number `file`, read-only and write-protected.
fn map_of(file: u64, id: u64, page: usize, at: usize, generation: u64) -> Frame {
    Frame {
        kind: wire::MAP,
        id,
        offset: (page * PAGE) as u64,
        len: PAGE as u64,
        backing: (at * PAGE) as u64,
        generation,
        flags: (file << 1) | wire::IMMUTABLE,
    }
}

// A read-only file is mapped private, and only immutable. A MAP that asks to
// write it, names a file the session was never given, or reaches past the
// length stated for its file changes nothing and is refused. A FILE that
// repeats the number at a larger length grows the file, and a DROP_FILE
// closes its descriptor, after which a MAP of it is refused too.
#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_read_only_file_maps_privately_grows_and_drops() {
    const NAME: &str = "sproutfs-files-read-only";
    let shared = named_file(c"sproutfs-files-read-only", PAGE, 4);
    serve_files(4, |socket| {
        file_for(1, PAGE, 2 * PAGE)
            .send_fd(socket, &read_only(&shared))
            .unwrap();
        acknowledge(socket, map_of(1, 2, 0, 1, 1), 0);
        assert_eq!(mappings_of(NAME), vec![(PAGE as u64, 'p')]);

        let writable = Frame {
            flags: 1 << 1,
            ..map_of(1, 3, 1, 0, 1)
        };
        acknowledge(socket, writable, libc::EINVAL);
        acknowledge(socket, map_of(2, 3, 1, 0, 1), libc::EINVAL);
        acknowledge(socket, map_of(1, 3, 1, 2, 1), libc::EINVAL);

        file_for(1, PAGE, 4 * PAGE)
            .send_fd(socket, &read_only(&shared))
            .unwrap();
        acknowledge(socket, map_of(1, 3, 1, 3, 1), 0);
        assert_eq!(
            mappings_of(NAME),
            vec![(PAGE as u64, 'p'), (3 * PAGE as u64, 'p')]
        );
        // This process is the pager too: it holds the file read-write, and the
        // session holds the one read-only descriptor the second FILE replaced
        // the first with.
        assert_eq!(descriptors_of(NAME), 2);

        acknowledge(
            socket,
            Frame {
                kind: wire::REVOKE,
                id: 4,
                len: 2 * PAGE as u64,
                generation: 2,
                ..Frame::default()
            },
            0,
        );
        assert_eq!(mappings_of(NAME), vec![]);
        Frame {
            kind: wire::DROP_FILE,
            id: 1,
            ..Frame::default()
        }
        .write(socket)
        .unwrap();
        acknowledge(socket, map_of(1, 5, 0, 0, 3), libc::EINVAL);
        assert_eq!(descriptors_of(NAME), 1);
        stop(socket);
    })
    .unwrap();
}

// The private file's mappings are shared and a read-only file's are private,
// and one span of a batch may hold runs of both.
#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_span_maps_each_run_as_its_file_is_mapped() {
    const RUNS: usize = 16;
    let shared = named_file(c"sproutfs-files-span", PAGE, RUNS);
    let private = named_file(c"sproutfs-files-span-private", PAGE, RUNS);
    serve_backing(
        spec(RUNS),
        attachment_for(PAGE),
        file_for(wire::PRIVATE_FILE, PAGE, RUNS * PAGE),
        &private,
        |socket| {
            file_for(1, PAGE, RUNS * PAGE)
                .send_fd(socket, &read_only(&shared))
                .unwrap();
            // Even pages come from the private file backwards and odd pages from
            // the read-only file forwards, so no two runs merge.
            let runs: Vec<_> = (0..RUNS)
                .map(|k| {
                    if k % 2 == 0 {
                        map_of(wire::PRIVATE_FILE, 2, k, RUNS - 1 - k, 1)
                    } else {
                        map_of(1, 2, k, k, 1)
                    }
                })
                .collect();
            assert_eq!(
                batch(
                    socket,
                    Frame {
                        kind: wire::MAP_BATCH,
                        id: 2,
                        len: RUNS as u64,
                        ..Frame::default()
                    },
                    &runs
                ),
                Some(Frame {
                    kind: wire::ACK,
                    id: 2,
                    ..Frame::default()
                })
            );
            let private_runs: Vec<_> = (0..RUNS)
                .step_by(2)
                .map(|k| (((RUNS - 1 - k) * PAGE) as u64, 's'))
                .collect();
            let shared_runs: Vec<_> = (1..RUNS)
                .step_by(2)
                .map(|k| ((k * PAGE) as u64, 'p'))
                .collect();
            assert_eq!(mappings_of("sproutfs-files-span-private"), private_runs);
            assert_eq!(mappings_of("sproutfs-files-span"), shared_runs);
            stop(socket);
        },
    )
    .unwrap();
}

/// What a peer sends to end a session: given the socket and the read-write
/// descriptor of a four-page file that is not the private one.
type Ending = fn(&mut UnixStream, &OwnedFd);

// A file this client cannot take as stated, or a drop it cannot make, is a
// pager it does not understand. It ends the session without an
// acknowledgement, and says so as invalid data.
#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_file_the_client_cannot_take_ends_the_session() {
    let cases: [(&str, Ending); 15] = [
        (
            "a read-only file with a read-write descriptor",
            |socket, file| {
                file_for(1, PAGE, PAGE).send_fd(socket, file).unwrap();
            },
        ),
        (
            "a file other than the private one stated writable",
            |socket, file| {
                Frame {
                    flags: wire::FILE_WRITABLE,
                    ..file_for(1, PAGE, PAGE)
                }
                .send_fd(socket, &read_only(file))
                .unwrap();
            },
        ),
        ("a file of the other arena kind", |socket, file| {
            Frame {
                backing: BACKING_HUGETLB,
                ..file_for(1, PAGE, PAGE)
            }
            .send_fd(socket, &read_only(file))
            .unwrap();
        }),
        ("a file longer than its descriptor", |socket, file| {
            file_for(1, PAGE, 8 * PAGE)
                .send_fd(socket, &read_only(file))
                .unwrap();
        }),
        ("a file that is not whole pages", |socket, file| {
            file_for(1, PAGE, PAGE - 1)
                .send_fd(socket, &read_only(file))
                .unwrap();
        }),
        ("an empty file", |socket, file| {
            file_for(1, PAGE, 0)
                .send_fd(socket, &read_only(file))
                .unwrap();
        }),
        ("a file with an offset", |socket, file| {
            Frame {
                offset: PAGE as u64,
                ..file_for(1, PAGE, PAGE)
            }
            .send_fd(socket, &read_only(file))
            .unwrap();
        }),
        ("a file with an unknown flag", |socket, file| {
            Frame {
                flags: 2,
                ..file_for(1, PAGE, PAGE)
            }
            .send_fd(socket, &read_only(file))
            .unwrap();
        }),
        ("a file without its descriptor", |socket, _| {
            file_for(1, PAGE, PAGE).write(socket).unwrap();
        }),
        ("a file repeated at the same length", |socket, file| {
            for _ in 0..2 {
                file_for(1, PAGE, PAGE)
                    .send_fd(socket, &read_only(file))
                    .unwrap();
            }
        }),
        ("a file repeated as another file", |socket, file| {
            file_for(1, PAGE, PAGE)
                .send_fd(socket, &read_only(file))
                .unwrap();
            let other = named_file(c"sproutfs-files-other", PAGE, 4);
            file_for(1, PAGE, 2 * PAGE)
                .send_fd(socket, &read_only(&other))
                .unwrap();
        }),
        ("a drop of the private file", |socket, _| {
            Frame {
                kind: wire::DROP_FILE,
                id: wire::PRIVATE_FILE,
                ..Frame::default()
            }
            .write(socket)
            .unwrap();
        }),
        ("a drop of a file never given", |socket, _| {
            Frame {
                kind: wire::DROP_FILE,
                id: 3,
                ..Frame::default()
            }
            .write(socket)
            .unwrap();
        }),
        ("a drop with a field set", |socket, file| {
            file_for(1, PAGE, PAGE)
                .send_fd(socket, &read_only(file))
                .unwrap();
            Frame {
                kind: wire::DROP_FILE,
                id: 1,
                len: PAGE as u64,
                ..Frame::default()
            }
            .write(socket)
            .unwrap();
        }),
        ("a mapping command carrying a descriptor", |socket, file| {
            map_of(wire::PRIVATE_FILE, 2, 0, 0, 1)
                .send_fd(socket, file)
                .unwrap();
        }),
    ];
    for (name, ending) in cases {
        let file = named_file(c"sproutfs-files-refused", PAGE, 4);
        let error = serve_files(4, |socket| {
            ending(socket, &file);
            // Half-close makes a session that wrongly took the frame read end
            // of file, which is not the refusal this asserts.
            socket.shutdown(std::net::Shutdown::Write).unwrap();
            assert!(Frame::read(socket).is_err(), "{name} was acknowledged");
        })
        .expect_err(name);
        assert_eq!(error.kind(), io::ErrorKind::InvalidData, "{name}: {error}");
    }
}

// The private file is part of the attachment: a session whose private file it
// cannot map as stated, or that is told READY before it has one, ends before
// the memory region is exposed.
#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_private_file_the_client_cannot_map_ends_the_attach() {
    let private = named_file(c"sproutfs-files-attach", PAGE, 1);
    let valid = file_for(wire::PRIVATE_FILE, PAGE, PAGE);
    let read_only_private = read_only(&private);
    let other_kind = named_file(c"sproutfs-files-attach-huge", MAX_PAGE_SIZE, 1);
    for (name, file, descriptor) in [
        (
            "a private file stated read-only",
            Frame { flags: 0, ..valid },
            &private,
        ),
        (
            "a private file with a read-only descriptor",
            valid,
            &read_only_private,
        ),
        (
            "a private file longer than its descriptor",
            Frame {
                len: 2 * PAGE as u64,
                ..valid
            },
            &private,
        ),
        ("an empty private file", Frame { len: 0, ..valid }, &private),
        (
            "a private file of the other arena kind",
            Frame {
                backing: BACKING_HUGETLB,
                ..valid
            },
            &private,
        ),
        (
            "a private file whose descriptor is the other memory",
            valid,
            &other_kind,
        ),
        (
            "a writable file that is not the private file",
            Frame { id: 1, ..valid },
            &private,
        ),
    ] {
        let error =
            handshake_with_backing(attachment_for(PAGE), file, ready(), spec(1), descriptor)
                .err()
                .unwrap_or_else(|| panic!("{name} was accepted"));
        assert_eq!(error.kind(), io::ErrorKind::InvalidData, "{name}: {error}");
    }

    let (result, _) = connect_with(spec(1), |socket| {
        attachment_for(PAGE).write(socket).unwrap();
        ready().write(socket).unwrap();
        Frame::read(socket).ok()
    });
    let error = result
        .err()
        .expect("READY before the private file was accepted");
    assert_eq!(error.kind(), io::ErrorKind::InvalidData, "{error}");

    let (result, _) = connect_with(spec(1), |socket| {
        attachment_for(PAGE).send_fd(socket, &private).unwrap();
    });
    let error = result
        .err()
        .expect("an attachment with a descriptor was accepted");
    assert_eq!(
        (error.kind(), error.to_string()),
        (
            io::ErrorKind::InvalidData,
            "the pager sent 1 descriptors with its attachment, which carries none".to_owned()
        )
    );

    // A pager that refuses the memory region closes the session, and the
    // error says it closed before attaching.
    let (result, _) = connect_with(spec(1), |_| {});
    let error = result.err().expect("a closed session attached");
    assert_eq!(
        (error.kind(), error.to_string()),
        (
            io::ErrorKind::UnexpectedEof,
            "before attaching: the pager closed the session".to_owned()
        )
    );
}
