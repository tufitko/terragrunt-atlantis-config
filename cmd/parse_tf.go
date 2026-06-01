package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/gruntwork-io/terragrunt/util"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
)

var localModuleSourcePrefixes = []string{
	"./",
	"../",
	".\\",
	"..\\",
}

func parseTerraformLocalModuleSource(path string) ([]string, error) {
	module, diags := tfconfig.LoadModule(path)
	// modules, diags := parser.loadConfigDir(path)
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}

	var sourceMap = map[string]bool{}
	for _, mc := range module.ModuleCalls {
		if isLocalTerraformModuleSource(mc.Source) {
			modulePath := util.JoinPath(path, mc.Source)

			// if modulePath is a symlink, dereference it to its real target
			if resolved, err := resolveSymlink(modulePath); err != nil {
				return nil, err
			} else {
				modulePath = resolved
			}

			modulePathGlob := util.JoinPath(modulePath, "*.tf*")

			if _, exists := sourceMap[modulePathGlob]; exists {
				continue
			}
			sourceMap[modulePathGlob] = true

			// find local module source recursively
			subSources, err := parseTerraformLocalModuleSource(modulePath)
			if err != nil {
				return nil, err
			}

			for _, subSource := range subSources {
				sourceMap[subSource] = true
			}
		}
	}

	var sources = []string{}
	for source := range sourceMap {
		sources = append(sources, source)
	}

	return sources, nil
}

// resolveSymlink returns the real target of path if it is a symlink,
// otherwise it returns path unchanged. A non-existent path is also
// returned unchanged.
func resolveSymlink(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", err
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}

	return filepath.EvalSymlinks(path)
}

func isLocalTerraformModuleSource(raw string) bool {
	for _, prefix := range localModuleSourcePrefixes {
		if strings.HasPrefix(raw, prefix) {
			return true
		}
	}

	return false
}
