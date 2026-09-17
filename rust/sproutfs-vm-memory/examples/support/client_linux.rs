//! The Linux body of the client example. It is a module of examples/client.rs
//! rather than the example itself so that the example's only unconditional
//! item is a main that says why there is nothing to run off Linux.

use sproutfs_vm_memory::{Region, RegionKind, RegionSpec, Session};
use std::io::{self, BufRead, Write};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU8, AtomicU64, Ordering};
use std::thread;

#[path = "kvm.rs"]
mod kvm;

fn range(regions: &[Region], words: &[&str]) -> (usize, usize) {
    let region = regions[words[1].parse::<usize>().unwrap()];
    let offset = words[2].parse::<usize>().unwrap();
    let len = words[3].parse::<usize>().unwrap();
    assert!(offset.checked_add(len).is_some_and(|end| end <= region.len));
    (region.address + offset, len)
}

fn load(address: usize) -> u8 {
    // The session stays alive until this process has stopped all memory users.
    // One-byte atomics avoid data races between the harness's CPU workers.
    unsafe { (&*(address as *const AtomicU8)).load(Ordering::SeqCst) }
}

pub fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<_> = std::env::args().collect();
    let page_size = sproutfs_vm_memory::PAGE_SIZE;
    let pages: usize = args.get(3).map(|s| s.parse().unwrap()).unwrap_or(1);
    // The sessions connect in region order, which is the order the pager accepts
    // them in.
    let mut sessions = Vec::new();
    for (index, kind) in [RegionKind::Pmem, RegionKind::Ram].into_iter().enumerate() {
        sessions.push(Session::connect(
            &args[1 + index],
            RegionSpec {
                kind,
                len: pages * page_size,
            },
        )?);
    }
    let regions: Vec<Region> = sessions.iter().map(Session::region).collect();
    let controls: Vec<_> = sessions.iter().map(Session::control).collect();
    let mut machine = None;
    let mut counter = None;
    let services: Vec<_> = sessions
        .into_iter()
        .map(|mut session| {
            thread::spawn(move || {
                if let Err(err) = session.run() {
                    eprintln!("mapping session failed: {err}");
                    // A test adapter cannot safely keep executing after control loss.
                    std::process::exit(2);
                }
            })
        })
        .collect();
    println!(
        "ready {} {} {} {} {}",
        std::process::id(),
        regions[0].address,
        regions[0].len,
        regions[1].address,
        regions[1].len
    );
    io::stdout().flush()?;
    for line in io::stdin().lock().lines() {
        let line = line?;
        let words: Vec<_> = line.split_whitespace().collect();
        match words.first().copied() {
            Some("seal") => {
                controls[words[1].parse::<usize>()?]
                    .start_seal()?
                    .wait(std::time::Duration::from_secs(10))?;
                println!("sealed");
            }
            Some("kvmread") | Some("kvmwrite") => {
                let region: usize = words[1].parse()?;
                let offset: usize = words[2].parse()?;
                if machine.is_none() {
                    machine = Some(kvm::Machine::new(&regions)?);
                }
                let value = if words[0] == "kvmwrite" {
                    Some(words[3].parse::<u8>()?)
                } else {
                    None
                };
                let byte = machine.as_mut().unwrap().access(region, offset, value)?;
                println!("kvm {byte}");
            }
            Some("read") | Some("gup") => {
                let (address, len) = range(&regions, &words);
                let mut bytes = vec![0u8; len];
                if words[0] == "gup" {
                    let local = libc::iovec {
                        iov_base: bytes.as_mut_ptr().cast(),
                        iov_len: len,
                    };
                    let remote = libc::iovec {
                        iov_base: address as *mut _,
                        iov_len: len,
                    };
                    let n =
                        unsafe { libc::process_vm_readv(libc::getpid(), &local, 1, &remote, 1, 0) };
                    assert_eq!(
                        n,
                        len as isize,
                        "GUP read failed: {}",
                        io::Error::last_os_error()
                    );
                } else {
                    for (i, byte) in bytes.iter_mut().enumerate() {
                        *byte = load(address + i);
                    }
                }
                print!("data ");
                for byte in bytes {
                    print!("{byte:02x}");
                }
                println!();
            }
            Some("fill") => {
                let (address, len) = range(&regions, &words);
                let value: u8 = words[4].parse()?;
                for i in 0..len {
                    unsafe {
                        (&*((address + i) as *const AtomicU8)).store(value, Ordering::SeqCst);
                    }
                }
                println!("filled");
            }
            Some("racefill") => {
                // racefill REGION OFFSET LEN VALUE SCAN_OFFSET SCAN_LEN stores
                // VALUE into the first range while four threads keep reading
                // the second, which the store never touches: every byte they
                // read, before, during and after the store's fault, must be 0.
                let (address, len) = range(&regions, &words);
                let value: u8 = words[4].parse()?;
                let (scan, scan_len) = range(&regions, &[words[0], words[1], words[5], words[6]]);
                let done = Arc::new(AtomicBool::new(false));
                let started = Arc::new(std::sync::Barrier::new(5));
                let readers: Vec<_> = (0..4)
                    .map(|worker| {
                        let (done, started) = (done.clone(), started.clone());
                        thread::spawn(move || {
                            let mut passes = 0usize;
                            loop {
                                for offset in (worker..scan_len).step_by(4) {
                                    assert_eq!(load(scan + offset), 0, "raced byte at {offset}");
                                }
                                passes += 1;
                                if passes == 1 {
                                    started.wait();
                                }
                                if done.load(Ordering::SeqCst) {
                                    return passes;
                                }
                            }
                        })
                    })
                    .collect();
                started.wait();
                for i in 0..len {
                    unsafe {
                        (&*((address + i) as *const AtomicU8)).store(value, Ordering::SeqCst);
                    }
                }
                done.store(true, Ordering::SeqCst);
                for reader in readers {
                    reader.join().unwrap();
                }
                println!("raced");
            }
            Some("stridefill") | Some("stridescan") => {
                let (address, len) = range(&regions, &words);
                let stride: usize = words[4].parse()?;
                let value: u8 = words[5].parse()?;
                assert!(stride > 0);
                for offset in (0..len).step_by(stride) {
                    if words[0] == "stridefill" {
                        unsafe {
                            (&*((address + offset) as *const AtomicU8))
                                .store(value, Ordering::SeqCst);
                        }
                    } else {
                        assert_eq!(load(address + offset), value, "strided byte at {offset}");
                    }
                }
                println!("strided");
            }
            Some("scan") => {
                let (address, len) = range(&regions, &words);
                let expected: u8 = words[4].parse()?;
                let iterations: usize = words[5].parse()?;
                let workers: Vec<_> = (0..4)
                    .map(|worker| {
                        thread::spawn(move || {
                            for i in 0..iterations {
                                assert_eq!(load(address + (i * 31 + worker) % len), expected);
                            }
                        })
                    })
                    .collect();
                for worker in workers {
                    worker.join().unwrap();
                }
                println!("scanned");
            }
            Some("start-counter") => {
                let (address, len) = range(&regions, &words);
                assert_eq!(len, 8);
                assert_eq!(address % 8, 0);
                assert!(counter.is_none());
                let done = Arc::new(AtomicBool::new(false));
                let worker_done = done.clone();
                let worker = thread::spawn(move || {
                    let mut count = 0u64;
                    while !worker_done.load(Ordering::Relaxed) {
                        unsafe {
                            (&*(address as *const AtomicU64)).fetch_add(1, Ordering::SeqCst);
                        }
                        count += 1;
                    }
                    count
                });
                counter = Some((address, done, worker));
                println!("counter-started");
            }
            Some("counter-progress") => {
                let address = counter.as_ref().unwrap().0;
                let value = unsafe { (&*(address as *const AtomicU64)).load(Ordering::SeqCst) };
                println!("counter-value {value}");
            }
            Some("stop-counter") => {
                let (_, done, worker) = counter.take().unwrap();
                done.store(true, Ordering::Relaxed);
                println!("counter-stopped {}", worker.join().unwrap());
            }
            Some("quit") => {
                assert!(counter.is_none(), "stop memory workers before shutdown");
                drop(machine.take()); // unregister KVM slots before session teardown
                // No users remain. The test now sends STOP on every control socket.
                println!("quiescent");
                io::stdout().flush()?;
                for service in services {
                    service.join().unwrap();
                }
                println!("bye");
                return Ok(());
            }
            _ => return Err("unknown test command".into()),
        }
        io::stdout().flush()?;
    }
    // EOF without the shutdown handshake must not silently detach the pager.
    std::process::exit(2);
}
