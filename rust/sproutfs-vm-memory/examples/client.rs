//! A small process adapter used by the real Go pager tests in internal/vmtest/.
//!
//! It maps two regions, one PMEM and one RAM, as a VMM does: each is its own
//! managed-memory session over its own socket. Test commands name them 0 and 1.

#[cfg(target_os = "linux")]
#[path = "support/client_linux.rs"]
mod imp;

#[cfg(target_os = "linux")]
fn main() -> Result<(), Box<dyn std::error::Error>> {
    imp::main()
}

// The library this drives is #![cfg(target_os = "linux")] and the guest it
// faults is KVM's, so off Linux the example builds to this and nothing else.
// It is still built there, which is what keeps `cargo clippy --all-targets`
// honest about the rest of the crate.
#[cfg(not(target_os = "linux"))]
fn main() {
    eprintln!("the managed-memory client adapter needs Linux userfaultfd and KVM");
    std::process::exit(1);
}
