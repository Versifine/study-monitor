//go:build !windows

package mediaingest

import (
	"fmt"
	"os"
)

func platformFileStamp(_ string, info os.FileInfo) (string, int64, error) {
	return fmt.Sprintf("%#v", info.Sys()), 0, nil
}
