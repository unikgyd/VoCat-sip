//go:build !linux

package ims

import "net"

func configureProtectedTCPDialer(_ *net.Dialer) {}
func configureTCPReuseAddr(_ *net.Dialer)        {}
