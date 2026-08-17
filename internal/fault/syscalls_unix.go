//go:build !windows

package fault

import (
	"errors"
	"syscall"
)

// These predicates exist so the classification ladder in classify.go stays
// portable. ECONNRESET and EPIPE are deliberately matched at the errno level:
// net.Error.Temporary() does not report them, so unwrapping is the only way to
// tell "the peer reset us" from any other I/O failure.
//
// errors.Is works on these because syscall.Errno implements Is.

func isConnReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }

func isBrokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }

func isConnRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

func isUnreachable(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
}

func syscallName(err error) string {
	switch {
	case isConnReset(err):
		return "ECONNRESET"
	case isBrokenPipe(err):
		return "EPIPE"
	case isConnRefused(err):
		return "ECONNREFUSED"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "EHOSTUNREACH"
	case errors.Is(err, syscall.ENETUNREACH):
		return "ENETUNREACH"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "ETIMEDOUT"
	default:
		return ""
	}
}
