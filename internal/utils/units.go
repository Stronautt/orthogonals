package utils

// Binary size units. Untyped, so they multiply against int, int64 and uint64
// byte counts alike.
const (
	BytesPerKiB = 1 << 10
	BytesPerMiB = 1 << 20
	BytesPerGiB = 1 << 30
)

// GiB is b as a GiB figure for display. Float because integer division reads
// 3.9 GiB of free disk as 3, and remedies quote these numbers back to the user.
func GiB(b uint64) float64 { return float64(b) / BytesPerGiB }
