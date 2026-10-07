//go:build !linux && !darwin

package porter

func collect(s *Status, disk string) {}
