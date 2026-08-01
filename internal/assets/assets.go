package assets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func Validate(root, model string) error {
	var problems []error
	need := []string{"kernel/vmlinux", "rootfs/rootfs.ext4", filepath.Join("models", model), "models.ext4"}
	for _, rel := range need {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			problems = append(problems, fmt.Errorf("missing asset: %s", rel))
		}
	}
	fc := filepath.Join(root, "bin/firecracker")
	if info, err := os.Stat(fc); err != nil {
		problems = append(problems, errors.New("missing asset: bin/firecracker"))
	} else if info.Mode()&0o111 == 0 {
		problems = append(problems, errors.New("bin/firecracker is not executable"))
	}
	if len(problems) > 0 {
		return fmt.Errorf("bundle at %s incomplete:\n%w", root, errors.Join(problems...))
	}
	return nil
}
