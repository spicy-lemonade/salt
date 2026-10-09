//go:build windows

package regular

import "os"

// nonBlock is not needed on Windows, where a path in a folder is never a
// named pipe that waits for a writer.
const nonBlock = 0

func setBlocking(*os.File) error { return nil }
