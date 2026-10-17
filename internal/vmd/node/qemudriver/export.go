package qemudriver

// OSCommander returns the Commander that runs real host commands (qemu-img, genisoimage, iptables)
// and starts QEMU detached. Daemons use it; tests inject fakes instead.
func OSCommander() Commander { return osCommander{} }
