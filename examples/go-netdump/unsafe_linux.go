//go:build linux

package main

import "unsafe"

// unsafePtr reinterprets a netlink message body as its fixed-size header.
func unsafePtr(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }

func nativeUint32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return *(*uint32)(unsafe.Pointer(&b[0]))
}
