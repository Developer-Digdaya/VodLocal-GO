package transfer

import (
	"strconv"
	"strings"
)

// parseMountInfo mengambil mount point dan tipe filesystem dari satu baris
// /proc/self/mountinfo.
//
// Baris bind mount dari sebuah subdirektori (root != "/") ditolak: systemd
// membuat bind mount seperti itu untuk ReadWritePaths= (mis. /run/media/
// vodlocal saat ProtectSystem=strict), dan mount tersebut bukan drive yang
// bisa dipakai untuk transfer.
func parseMountInfo(line string) (mountpoint, fstype string, ok bool) {
	fields := strings.Fields(line)
	sep := -1
	for i, field := range fields {
		if field == "-" {
			sep = i
			break
		}
	}
	if sep < 6 || len(fields) < sep+3 {
		return "", "", false
	}
	if unescapeMount(fields[3]) != "/" {
		return "", "", false
	}
	return unescapeMount(fields[4]), fields[sep+1], true
}

// unescapeMount decodes the octal escapes used by /proc/self/mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
