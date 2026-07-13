package driver

import (
	"fmt"
	"path/filepath"
	"strings"
)

type mountPointObservation struct {
	Mounted  bool
	Readonly bool
}

var mountInfoUnescaper = strings.NewReplacer(
	`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, "\x00",
)

func parseMountPointObservation(body []byte, target string) (mountPointObservation, error) {
	target = filepath.Clean(target)
	var observation mountPointObservation
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint, err := unescapeMountInfoField(fields[4])
		if err != nil {
			return mountPointObservation{}, fmt.Errorf("decode mountpoint: %w", err)
		}
		if filepath.Clean(mountPoint) != target {
			continue
		}
		if observation.Mounted {
			return mountPointObservation{}, fmt.Errorf("multiple mountinfo records for target %q", target)
		}
		if len(fields) < 6 {
			return mountPointObservation{}, fmt.Errorf("mountinfo record for target %q has no mount options", target)
		}
		options := "," + fields[5] + ","
		readonly := strings.Contains(options, ",ro,")
		writable := strings.Contains(options, ",rw,")
		if readonly == writable {
			return mountPointObservation{}, fmt.Errorf("mountinfo record for target %q has ambiguous readonly mode", target)
		}
		observation = mountPointObservation{Mounted: true, Readonly: readonly}
	}
	return observation, nil
}

func unescapeMountInfoField(value string) (string, error) {
	decoded := mountInfoUnescaper.Replace(value)
	if strings.ContainsRune(decoded, '\\') {
		return "", fmt.Errorf("unsupported or truncated escape in %q", value)
	}
	return strings.ReplaceAll(decoded, "\x00", `\`), nil
}
