package operations

import "errors"

const (
	DiskNormal   = "normal"
	DiskWarning  = "warning"
	DiskCritical = "critical"
	DiskReserve  = "reserve"
)

type DiskProbe interface{ FreeBytes(string) (uint64, error) }

type fixedProbe struct {
	bytes uint64
	err   error
}

func (probe fixedProbe) FreeBytes(string) (uint64, error) { return probe.bytes, probe.err }

var errDiskProbe = errors.New("disk free-space probe failed")
