package hwinfo

import (
	"os/exec"
	"strings"
)

func detect() Specs {
	return sysctlSpecs(func(name string) string {
		out, err := exec.Command("sysctl", "-n", name).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	})
}
