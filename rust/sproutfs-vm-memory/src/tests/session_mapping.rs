use super::*;

fn vm_flags(address: usize) -> Vec<String> {
    let maps = std::fs::read_to_string("/proc/self/smaps").unwrap();
    let mut contains_address = false;
    for line in maps.lines() {
        if let Some(range) = line
            .split_whitespace()
            .next()
            .and_then(|word| word.split_once('-'))
        {
            let start = usize::from_str_radix(range.0, 16).unwrap();
            let end = usize::from_str_radix(range.1, 16).unwrap();
            contains_address = start <= address && address < end;
        } else if contains_address && line.starts_with("VmFlags:") {
            return line.split_whitespace().skip(1).map(str::to_owned).collect();
        }
    }
    panic!("no mapping flags for {address:#x}");
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn exposed_trap_ranges_disable_fork_inheritance_and_transparent_huge_pages() {
    let spec = RegionSpec {
        kind: RegionKind::Ram,
        len: PAGE_SIZE,
    };
    let session = handshake(attachment(), ready(), spec).unwrap();
    let region = session.region();
    for address in [region.address, region.address + region.len - 1] {
        let flags = vm_flags(address);
        assert!(
            flags.iter().any(|flag| flag == "dc"),
            "mapping must not be inherited by fork: {flags:?}"
        );
        assert!(
            flags.iter().any(|flag| flag == "nh"),
            "anonymous traps must disable THP: {flags:?}"
        );
    }
}
