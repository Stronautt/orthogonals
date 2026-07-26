package utils

// Binary size units. Untyped, so they multiply against int, int64 and uint64
// byte counts alike without a conversion at each site.
const (
	BytesPerKiB = 1 << 10
	BytesPerMiB = 1 << 20
	BytesPerGiB = 1 << 30
)

// GiB is b as a GiB figure for display. Float because integer division reads
// 3.9 GiB of free disk as 3, and these numbers are quoted back to the user in
// remedies telling them how much more they need.
func GiB(b uint64) float64 { return float64(b) / BytesPerGiB }
