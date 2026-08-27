//go:build !linux

package main

import "errors"

// The guest target is Linux/Unikraft; these stubs only keep the example
// buildable on a developer machine.
func dumpRoutes() ([]Route, error) { return nil, errors.New("route dump unsupported on this platform") }

func dumpNeighbours() ([]Neighbour, error) {
	return nil, errors.New("neighbour dump unsupported on this platform")
}
