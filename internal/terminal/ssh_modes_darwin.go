//go:build darwin

package terminal

import (
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func SSHTerminalModes(file *os.File) ssh.TerminalModes {
	modes := ssh.TerminalModes{}
	if file == nil {
		return modes
	}
	termios, err := unix.IoctlGetTermios(int(file.Fd()), unix.TIOCGETA)
	if err != nil {
		return modes
	}
	addControlModes(modes, termios.Cc[:], map[uint8]int{
		ssh.VINTR:    unix.VINTR,
		ssh.VQUIT:    unix.VQUIT,
		ssh.VERASE:   unix.VERASE,
		ssh.VKILL:    unix.VKILL,
		ssh.VEOF:     unix.VEOF,
		ssh.VEOL:     unix.VEOL,
		ssh.VEOL2:    unix.VEOL2,
		ssh.VSTART:   unix.VSTART,
		ssh.VSTOP:    unix.VSTOP,
		ssh.VSUSP:    unix.VSUSP,
		ssh.VDSUSP:   unix.VDSUSP,
		ssh.VREPRINT: unix.VREPRINT,
		ssh.VWERASE:  unix.VWERASE,
		ssh.VLNEXT:   unix.VLNEXT,
		ssh.VSTATUS:  unix.VSTATUS,
		ssh.VDISCARD: unix.VDISCARD,
	})
	addFlagModes(modes, termios.Iflag, map[uint8]uint64{
		ssh.IGNPAR:  unix.IGNPAR,
		ssh.PARMRK:  unix.PARMRK,
		ssh.INPCK:   unix.INPCK,
		ssh.ISTRIP:  unix.ISTRIP,
		ssh.INLCR:   unix.INLCR,
		ssh.IGNCR:   unix.IGNCR,
		ssh.ICRNL:   unix.ICRNL,
		ssh.IXON:    unix.IXON,
		ssh.IXANY:   unix.IXANY,
		ssh.IXOFF:   unix.IXOFF,
		ssh.IMAXBEL: unix.IMAXBEL,
	})
	addFlagModes(modes, termios.Lflag, map[uint8]uint64{
		ssh.ISIG:    unix.ISIG,
		ssh.ICANON:  unix.ICANON,
		ssh.ECHO:    unix.ECHO,
		ssh.ECHOE:   unix.ECHOE,
		ssh.ECHOK:   unix.ECHOK,
		ssh.ECHONL:  unix.ECHONL,
		ssh.NOFLSH:  unix.NOFLSH,
		ssh.TOSTOP:  unix.TOSTOP,
		ssh.IEXTEN:  unix.IEXTEN,
		ssh.ECHOCTL: unix.ECHOCTL,
		ssh.ECHOKE:  unix.ECHOKE,
		ssh.PENDIN:  unix.PENDIN,
	})
	addFlagModes(modes, termios.Oflag, map[uint8]uint64{
		ssh.OPOST:  unix.OPOST,
		ssh.ONLCR:  unix.ONLCR,
		ssh.OCRNL:  unix.OCRNL,
		ssh.ONOCR:  unix.ONOCR,
		ssh.ONLRET: unix.ONLRET,
	})
	addFlagModes(modes, termios.Cflag, map[uint8]uint64{
		ssh.CS7:    unix.CS7,
		ssh.CS8:    unix.CS8,
		ssh.PARENB: unix.PARENB,
		ssh.PARODD: unix.PARODD,
	})
	modes[ssh.TTY_OP_ISPEED] = uint32(termios.Ispeed)
	modes[ssh.TTY_OP_OSPEED] = uint32(termios.Ospeed)
	return modes
}
