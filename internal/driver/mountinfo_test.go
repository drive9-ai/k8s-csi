package driver

import "testing"

func TestParseMountPointObservation(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		target       string
		wantMounted  bool
		wantReadonly bool
		wantErr      bool
	}{
		{
			name:   "target absent",
			body:   "36 35 0:32 / /other rw,noatime - fuse.drive9 drive9 rw\n",
			target: "/target",
		},
		{
			name:         "writable per-mount options",
			body:         "36 35 0:32 / /target rw,noatime - fuse.drive9 drive9 rw\n",
			target:       "/target",
			wantMounted:  true,
			wantReadonly: false,
		},
		{
			name:         "readonly per-mount options",
			body:         "36 35 0:32 / /target ro,relatime - fuse.drive9 drive9 rw\n",
			target:       "/target",
			wantMounted:  true,
			wantReadonly: true,
		},
		{
			name:         "space escape",
			body:         "36 35 0:32 / /var/lib/drive9\\040workspace ro - fuse.drive9 drive9 rw\n",
			target:       "/var/lib/drive9 workspace",
			wantMounted:  true,
			wantReadonly: true,
		},
		{
			name:         "tab escape",
			body:         "36 35 0:32 / /var/lib/drive9\\011workspace ro - fuse.drive9 drive9 rw\n",
			target:       "/var/lib/drive9\tworkspace",
			wantMounted:  true,
			wantReadonly: true,
		},
		{
			name:         "newline escape",
			body:         "36 35 0:32 / /var/lib/drive9\\012workspace ro - fuse.drive9 drive9 rw\n",
			target:       "/var/lib/drive9\nworkspace",
			wantMounted:  true,
			wantReadonly: true,
		},
		{
			name:         "backslash escape",
			body:         "36 35 0:32 / /var/lib/drive9\\134workspace ro - fuse.drive9 drive9 rw\n",
			target:       "/var/lib/drive9\\workspace",
			wantMounted:  true,
			wantReadonly: true,
		},
		{
			name:         "backslash octal text decoded once",
			body:         "36 35 0:32 / /var/lib/drive9\\134040workspace ro - fuse.drive9 drive9 rw\n",
			target:       "/var/lib/drive9\\040workspace",
			wantMounted:  true,
			wantReadonly: true,
		},
		{
			name:         "superblock readonly does not override per-mount writable",
			body:         "36 35 0:32 / /target rw,noatime - fuse.drive9 drive9 ro\n",
			target:       "/target",
			wantMounted:  true,
			wantReadonly: false,
		},
		{
			name:    "matching record missing mount options",
			body:    "36 35 0:32 / /target\n",
			target:  "/target",
			wantErr: true,
		},
		{
			name:    "matching record missing readonly mode",
			body:    "36 35 0:32 / /target noatime - fuse.drive9 drive9 rw\n",
			target:  "/target",
			wantErr: true,
		},
		{
			name:    "matching record has truncated escape",
			body:    "36 35 0:32 / /target\\13 ro - fuse.drive9 drive9 rw\n",
			target:  "/target",
			wantErr: true,
		},
		{
			name:    "matching record has unsupported escape",
			body:    "36 35 0:32 / /target\\777 ro - fuse.drive9 drive9 rw\n",
			target:  "/target",
			wantErr: true,
		},
		{
			name:    "matching record has both modes",
			body:    "36 35 0:32 / /target ro,rw - fuse.drive9 drive9 rw\n",
			target:  "/target",
			wantErr: true,
		},
		{
			name: "duplicate matching records are ambiguous",
			body: "36 35 0:32 / /target ro - fuse.drive9 drive9 rw\n" +
				"37 36 0:32 / /target ro - fuse.drive9 drive9 rw\n",
			target:  "/target",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMountPointObservation([]byte(test.body), test.target)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseMountPointObservation() error = %v, wantErr %t", err, test.wantErr)
			}
			if got.Mounted != test.wantMounted {
				t.Fatalf("Mounted = %t, want %t", got.Mounted, test.wantMounted)
			}
			if got.Readonly != test.wantReadonly {
				t.Fatalf("Readonly = %t, want %t", got.Readonly, test.wantReadonly)
			}
		})
	}
}
