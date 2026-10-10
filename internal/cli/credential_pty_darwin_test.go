package cli

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openCredentialTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "credential-pty-master")
	t.Cleanup(func() { master.Close() })
	for _, request := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(fd, request, 0); err != nil {
			t.Fatal(err)
		}
	}
	var name [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(unix.ByteSliceToString(name[:]), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func credentialTestEchoEnabled(fd int) (bool, error) {
	settings, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return false, err
	}
	return settings.Lflag&unix.ECHO != 0, nil
}
