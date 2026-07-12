//go:build !darwin && !linux

package main

// Stubs so the build succeeds on unsupported platforms; header shows zeros.
func initPlatformMetrics()           {}
func readCPUPercent() float64        { return 0 }
func readMem() (used, total uint64)  { return 0, 0 }
func readDisk() (used, total uint64) { return 0, 0 }
func readNetBytes() (rx, tx uint64)  { return 0, 0 }
