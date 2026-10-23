package transfer

import "testing"

func TestParseMountInfo(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantMount string
		wantFS    string
		wantOK    bool
	}{
		{
			name:      "USB exFAT mount",
			line:      `526 583 8:2 / /run/media/vodlocal/24FB-FCBF rw,nosuid,nodev,noexec,noatime shared:432 master:422 - exfat /dev/sda2 rw,uid=986,gid=986`,
			wantMount: "/run/media/vodlocal/24FB-FCBF",
			wantFS:    "exfat",
			wantOK:    true,
		},
		{
			name:      "USB vfat mount",
			line:      `514 583 8:1 / /run/media/vodlocal/67E3-17ED rw,nosuid,nodev,noexec,noatime shared:420 master:391 - vfat /dev/sda1 rw,uid=986,gid=986`,
			wantMount: "/run/media/vodlocal/67E3-17ED",
			wantFS:    "vfat",
			wantOK:    true,
		},
		{
			name:   "ReadWritePaths bind mount is not a drive",
			line:   `585 583 0:29 /media/vodlocal /run/media/vodlocal rw,nosuid,nodev shared:394 master:13 - tmpfs tmpfs rw,size=1075472k`,
			wantOK: false,
		},
		{
			name:      "escaped space in mount point",
			line:      `100 30 8:1 / /run/media/my\040disk rw,relatime - vfat /dev/sdb1 rw`,
			wantMount: "/run/media/my disk",
			wantFS:    "vfat",
			wantOK:    true,
		},
		{
			name:   "malformed line",
			line:   `garbage`,
			wantOK: false,
		},
		{
			name:   "missing separator",
			line:   `1 2 8:1 / /mnt/x rw`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mount, fsType, ok := parseMountInfo(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if mount != tt.wantMount {
				t.Errorf("mount = %q, want %q", mount, tt.wantMount)
			}
			if fsType != tt.wantFS {
				t.Errorf("fstype = %q, want %q", fsType, tt.wantFS)
			}
		})
	}
}
