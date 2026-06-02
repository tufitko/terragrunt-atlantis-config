package cmd

// Terragrunt doesn't give us an easy way to access all of the Locals from a module
// in an easy to digest way. This file is mostly just follows along how Terragrunt
// parses the `locals` blocks and evaluates their contents.

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/gruntwork-io/go-commons/errors"
	"github.com/gruntwork-io/terragrunt/config"
	"github.com/gruntwork-io/terragrunt/config/hclparse"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/sync/singleflight"
)

// ResolvedLocals are the parsed result of local values this module cares about
type ResolvedLocals struct {
	// The Atlantis workflow to use for some project
	AtlantisWorkflow string

	// Apply requirements to override the global `--apply-requirements` flag
	ApplyRequirements []string

	// Extra dependencies that can be hardcoded in config
	ExtraAtlantisDependencies []string

	// If set, a single module will have autoplan turned to this setting
	AutoPlan *bool

	// If set to true, the module will not be included in the output
	Skip *bool

	// Terraform version to use just for this project
	TerraformVersion string

	// The project output will not be included in the output for given atlantis commands
	SilencePRComments []string

	// If set to true, create Atlantis project
	markedProject *bool
}

// parseHcl uses the HCL2 parser to parse the given string into an HCL file body.
func parseHcl(parser *hclparse.Parser, hcl string, filename string) (file *hcl.File, err error) {
	// The HCL2 parser and especially cty conversions will panic in many types of errors, so we have to recover from
	// those panics here and convert them to normal errors
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.WithStackTrace(hclparse.PanicWhileParsingConfigError{RecoveredValue: recovered, ConfigFile: filename})
		}
	}()

	if filepath.Ext(filename) == ".json" {
		file, parseDiagnostics := parser.ParseJSON([]byte(hcl), filename)
		if parseDiagnostics != nil && parseDiagnostics.HasErrors() {
			return nil, parseDiagnostics
		}

		return file, nil
	}

	file, parseDiagnostics := parser.ParseHCL([]byte(hcl), filename)
	if parseDiagnostics != nil && parseDiagnostics.HasErrors() {
		return nil, parseDiagnostics
	}

	return file, nil
}

// Merges in values from a child into a parent set of `local` values
func mergeResolvedLocals(parent ResolvedLocals, child ResolvedLocals) ResolvedLocals {
	if child.AtlantisWorkflow != "" {
		parent.AtlantisWorkflow = child.AtlantisWorkflow
	}

	if child.TerraformVersion != "" {
		parent.TerraformVersion = child.TerraformVersion
	}

	if child.AutoPlan != nil {
		parent.AutoPlan = child.AutoPlan
	}

	if child.Skip != nil {
		parent.Skip = child.Skip
	}

	if child.markedProject != nil {
		parent.markedProject = child.markedProject
	}

	if child.ApplyRequirements != nil || len(child.ApplyRequirements) > 0 {
		parent.ApplyRequirements = child.ApplyRequirements
	}

	parent.ExtraAtlantisDependencies = append(parent.ExtraAtlantisDependencies, child.ExtraAtlantisDependencies...)

	if child.SilencePRComments != nil || len(child.SilencePRComments) > 0 {
		parent.SilencePRComments = child.SilencePRComments
	}

	return parent
}

// Set up a cache for the parseLocals function. The same parent config files
// (root.hcl, region.hcl, _commons/*.hcl, ...) are included by many projects, and
// resolving their locals via DecodeBaseBlocks is one of the most expensive steps.
// Without this cache each shared parent gets re-parsed once per descendant project.
type parseLocalsOutput struct {
	locals ResolvedLocals
	err    error
}

type parseLocalsCache struct {
	mtx  sync.RWMutex
	data map[string]parseLocalsOutput
}

func newParseLocalsCache() *parseLocalsCache {
	return &parseLocalsCache{data: map[string]parseLocalsOutput{}}
}

func (m *parseLocalsCache) get(k string) (parseLocalsOutput, bool) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()
	v, ok := m.data[k]
	return v, ok
}

func (m *parseLocalsCache) set(k string, v parseLocalsOutput) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.data[k] = v
}

var getParseLocalsCache = newParseLocalsCache()
var parseLocalsRequestGroup singleflight.Group

// Parses a given file, returning a map of all it's `local` values.
//
// Only top-level resolutions (includeFromChild == nil) are memoized. A parent
// config's locals can depend on the including child (e.g. via
// path_relative_to_include() or get_terragrunt_dir()), so caching parent-level
// calls keyed only by the parent path would return results from the wrong child.
// The top-level call for a given project path is, however, deterministic and is
// performed at least twice per project (once in getDependencies and once in
// createProject), so caching it removes that duplicated work.
func parseLocals(ctx *config.ParsingContext, path string, includeFromChild *config.IncludeConfig) (ResolvedLocals, error) {
	if includeFromChild != nil {
		return parseLocalsUncached(ctx, path, includeFromChild)
	}

	if cached, ok := getParseLocalsCache.get(path); ok {
		return cached.locals, cached.err
	}

	res, err, _ := parseLocalsRequestGroup.Do(path, func() (interface{}, error) {
		// Re-check the cache: a concurrent caller may have populated it while we
		// were waiting to enter the singleflight critical section.
		if cached, ok := getParseLocalsCache.get(path); ok {
			return cached.locals, cached.err
		}

		locals, err := parseLocalsUncached(ctx, path, includeFromChild)
		getParseLocalsCache.set(path, parseLocalsOutput{locals: locals, err: err})
		return locals, err
	})

	if err != nil {
		return ResolvedLocals{}, err
	}
	return res.(ResolvedLocals), nil
}

// parseLocalsUncached does the actual work of resolving a config file's locals,
// following Terragrunt's own evaluation of `locals` blocks and parent includes.
func parseLocalsUncached(ctx *config.ParsingContext, path string, includeFromChild *config.IncludeConfig) (ResolvedLocals, error) {
	file, err := hclparse.NewParser(ctx.ParserOptions...).ParseFromFile(path)
	if err != nil {
		return ResolvedLocals{}, err
	}

	// Decode just the Base blocks. See the function docs for DecodeBaseBlocks for more info on what base blocks are.
	baseBlocks, err := config.DecodeBaseBlocks(ctx, file, includeFromChild)
	if err != nil {
		return ResolvedLocals{}, err
	}

	// Recurse on the parent to merge in the locals from that file
	mergedParentLocals := ResolvedLocals{}
	if baseBlocks.TrackInclude != nil && includeFromChild == nil {
		for _, includeConfig := range baseBlocks.TrackInclude.CurrentList {
			parentLocals, _ := parseLocals(ctx, includeConfig.Path, &includeConfig)
			mergedParentLocals = mergeResolvedLocals(mergedParentLocals, parentLocals)
		}
	}
	childLocals, err := resolveLocals(*baseBlocks.Locals)
	if err != nil {
		return ResolvedLocals{}, err
	}
	return mergeResolvedLocals(mergedParentLocals, childLocals), nil
}

func resolveLocals(localsAsCty cty.Value) (ResolvedLocals, error) {
	resolved := ResolvedLocals{}

	// Return an empty set of locals if no `locals` block was present
	if localsAsCty == cty.NilVal {
		return resolved, nil
	}
	rawLocals := localsAsCty.AsValueMap()

	workflowValue, ok := rawLocals["atlantis_workflow"]
	if ok {
		resolved.AtlantisWorkflow = workflowValue.AsString()
	}

	versionValue, ok := rawLocals["atlantis_terraform_version"]
	if ok {
		resolved.TerraformVersion = versionValue.AsString()
	}

	autoPlanValue, ok := rawLocals["atlantis_autoplan"]
	if ok {
		hasValue := autoPlanValue.True()
		resolved.AutoPlan = &hasValue
	}

	skipValue, ok := rawLocals["atlantis_skip"]
	if ok {
		hasValue := skipValue.True()
		resolved.Skip = &hasValue
	}

	applyReqs, ok := rawLocals["atlantis_apply_requirements"]
	if ok {
		resolved.ApplyRequirements = []string{}
		it := applyReqs.ElementIterator()
		for it.Next() {
			_, val := it.Element()
			resolved.ApplyRequirements = append(resolved.ApplyRequirements, val.AsString())
		}
	}

	markedProject, ok := rawLocals["atlantis_project"]
	if ok {
		hasValue := markedProject.True()
		resolved.markedProject = &hasValue
	}

	extraDependenciesAsCty, ok := rawLocals["extra_atlantis_dependencies"]
	if ok {
		it := extraDependenciesAsCty.ElementIterator()
		for it.Next() {
			pos, val := it.Element()
			if !val.Type().Equals(cty.String) {
				posInt, _ := pos.AsBigFloat().Int64()
				return resolved, fmt.Errorf("extra_atlantis_dependencies contains non-string value at position %d", posInt)
			}

			resolved.ExtraAtlantisDependencies = append(
				resolved.ExtraAtlantisDependencies,
				filepath.ToSlash(val.AsString()),
			)
		}
	}

	silencePRComments, ok := rawLocals["atlantis_silence_pr_comments"]
	if ok {
		resolved.SilencePRComments = []string{}
		it := silencePRComments.ElementIterator()
		for it.Next() {
			pos, val := it.Element()
			if !val.Type().Equals(cty.String) {
				posInt, _ := pos.AsBigFloat().Int64()
				return resolved, fmt.Errorf("silence_pr_comments contains non-string value at position %d", posInt)
			}

			resolved.SilencePRComments = append(resolved.SilencePRComments, val.AsString())
		}
	}

	return resolved, nil
}
