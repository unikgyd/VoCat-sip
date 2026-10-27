//go:build !linux

package main

import "errors"

func runQMIProxy([]string) error { return errors.New("qmi-proxy service requires Linux") }
