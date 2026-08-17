//go:build windows

package fault

import (
	"errors"
	"syscall"
)

// Windows reports these conditions through WSA error codes. They are written
// as literals rather than pulled from golang.org/x/sys/windows to keep the
// dependency set minimal — these five numbers have been stable since Winsock 2.
const (
	wsaeNetUnreach  syscall.Errno = 10051
	wsaeConnAborted syscall.Errno = 10053
	wsaeConnReset   syscall.Errno = 10054
	wsaeConnRefused syscall.Errno = 10061
	wsaeHostUnreach syscall.Errno = 10065
)

func isConnReset(err error) bool {
	return errors.Is(err, wsaeConnReset) || errors.Is(err, syscall.ECONNRESET)
}

func isBrokenPipe(err error) bool {
	return errors.Is(err, wsaeConnAborted) || errors.Is(err, syscall.EPIPE)
}

func isConnRefused(err error) bool {
	return errors.Is(err, wsaeConnRefused) || errors.Is(err, syscall.ECONNREFUSED)
}

func isUnreachable(err error) bool {
	return errors.Is(err, wsaeHostUnreach) || errors.Is(err, wsaeNetUnreach)
}

func syscallName(err error) string {
	switch {
	case isConnReset(err):
		return "WSAECONNRESET"
	case isBrokenPipe(err):
		return "WSAECONNABORTED"
	case isConnRefused(err):
		return "WSAECONNREFUSED"
	case isUnreachable(err):
		return "WSAEHOSTUNREACH"
	default:
		return ""
	}
}
