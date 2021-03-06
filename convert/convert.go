// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package convert

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/hcl/token"
	"github.com/hashicorp/terraform/command"
	"github.com/hashicorp/terraform/svchost"
	"github.com/hashicorp/terraform/svchost/auth"
	"github.com/hashicorp/terraform/svchost/disco"
	"github.com/pkg/errors"

	"github.com/pulumi/tf2pulumi/gen"
	"github.com/pulumi/tf2pulumi/gen/nodejs"
	"github.com/pulumi/tf2pulumi/gen/python"
	"github.com/pulumi/tf2pulumi/il"
	"github.com/pulumi/tf2pulumi/internal/config/module"
)

const (
	LanguageTypescript string = "typescript"
	LanguagePython     string = "python"
)

var (
	ValidLanguages = [...]string{LanguageTypescript, LanguagePython}
)

func addLocationAnnotation(location token.Pos, comments **il.Comments) {
	if !location.IsValid() {
		return
	}

	c := *comments
	if c == nil {
		c = &il.Comments{}
		*comments = c
	}

	if len(c.Leading) != 0 {
		c.Leading = append(c.Leading, "")
	}
	c.Leading = append(c.Leading, fmt.Sprintf(" Originally defined at %v:%v", location.Filename, location.Line))
}

// addLocationAnnotations adds comments that record the original source location of each top-level node in a module.
func addLocationAnnotations(m *il.Graph) {
	for _, n := range m.Modules {
		addLocationAnnotation(n.Location, &n.Comments)
	}
	for _, n := range m.Providers {
		addLocationAnnotation(n.Location, &n.Comments)
	}
	for _, n := range m.Resources {
		addLocationAnnotation(n.Location, &n.Comments)
	}
	for _, n := range m.Outputs {
		addLocationAnnotation(n.Location, &n.Comments)
	}
	for _, n := range m.Locals {
		addLocationAnnotation(n.Location, &n.Comments)
	}
	for _, n := range m.Variables {
		addLocationAnnotation(n.Location, &n.Comments)
	}
}

// Convert converts a Terraform module at the provided location into a Pulumi module, written to stdout.
func Convert(opts Options) error {
	// Set default options where appropriate.
	if opts.Path == "" {
		opts.Path = "."
	}
	if opts.Writer == nil {
		opts.Writer = os.Stdout
	}

	services := disco.NewWithCredentialsSource(noCredentials{})
	moduleStorage := module.NewStorage(filepath.Join(command.DefaultDataDir, "modules"), services)

	mod, err := module.NewTreeModule("", opts.Path)
	if err != nil {
		return errors.Wrapf(err, "creating tree module")
	}

	if err = mod.Load(moduleStorage); err != nil {
		return errors.Wrapf(err, "loading module")
	}

	gs, err := buildGraphs(mod, true, opts)
	if err != nil {
		return errors.Wrapf(err, "importing Terraform project graphs")
	}

	// Filter resource name properties if requested.
	if opts.FilterResourceNames {
		filterAutoNames := opts.ResourceNameProperty == ""
		for _, g := range gs {
			for _, r := range g.Resources {
				if !r.IsDataSource {
					il.FilterProperties(r, func(key string, _ il.BoundNode) bool {
						if filterAutoNames {
							sch := r.Schemas().PropertySchemas(key).Pulumi
							return sch == nil || sch.Default == nil || !sch.Default.AutoNamed
						}
						return key != opts.ResourceNameProperty
					})
				}
			}
		}
	}

	// Annotate nodes with the location of their original definition if requested.
	if opts.AnnotateNodesWithLocations {
		for _, g := range gs {
			addLocationAnnotations(g)
		}
	}

	generator, err := newGenerator("auto", opts)
	if err != nil {
		return errors.Wrapf(err, "creating generator")
	}

	if err = gen.Generate(gs, generator); err != nil {
		return errors.Wrapf(err, "generating code")
	}

	return nil
}

type Options struct {
	// AllowMissingProviders, if true, allows code-gen to continue even if resource providers are missing.
	AllowMissingProviders bool
	// AllowMissingVariables, if true, allows code-gen to continue even if the input configuration references missing
	// variables.
	AllowMissingVariables bool
	// AllowMissingComments allows binding to succeed even if there are errors extracting comments from the source.
	AllowMissingComments bool
	// AnnotateNodesWithLocations is true if the generated source code should contain comments that annotate top-level
	// nodes with their original source locations.
	AnnotateNodesWithLocations bool
	// FilterResourceNames, if true, removes the property indicated by ResourceNameProperty from all resources in the
	// graph.
	FilterResourceNames bool
	// ResourceNameProperty sets the key of the resource name property that will be removed if FilterResourceNames is
	// true.
	ResourceNameProperty string
	// Path, when set, overrides the default path (".") to load the source Terraform module from.
	Path string
	// Writer can be set to override the default behavior of writing the resulting code to stdout.
	Writer io.Writer
	// Optional source for provider schema information.
	ProviderInfoSource il.ProviderInfoSource
	// Optional logger for diagnostic information.
	Logger *log.Logger
	// The target language.
	TargetLanguage string
	// The target SDK version.
	TargetSDKVersion string

	// TargetOptions captures any target-specific options.
	TargetOptions interface{}
}

type noCredentials struct{}

func (noCredentials) ForHost(host svchost.Hostname) (auth.HostCredentials, error) {
	return nil, nil
}

func (noCredentials) StoreForHost(host svchost.Hostname, credentials auth.HostCredentialsWritable) error {
	return nil
}

func (noCredentials) ForgetForHost(host svchost.Hostname) error {
	return nil
}

func buildGraphs(tree *module.Tree, isRoot bool, opts Options) ([]*il.Graph, error) {
	// TODO: move this into the il package and unify modules based on path

	children := []*il.Graph{}
	for _, c := range tree.Children() {
		cc, err := buildGraphs(c, false, opts)
		if err != nil {
			return nil, err
		}
		children = append(children, cc...)
	}

	buildOpts := il.BuildOptions{
		AllowMissingProviders: opts.AllowMissingProviders,
		AllowMissingVariables: opts.AllowMissingVariables,
		AllowMissingComments:  opts.AllowMissingComments,
		ProviderInfoSource:    opts.ProviderInfoSource,
		Logger:                opts.Logger,
	}
	g, err := il.BuildGraph(tree, &buildOpts)
	if err != nil {
		return nil, err
	}

	return append(children, g), nil
}

func newGenerator(projectName string, opts Options) (gen.Generator, error) {
	switch opts.TargetLanguage {
	case LanguageTypescript:
		nodeOpts, ok := opts.TargetOptions.(nodejs.Options)
		if !ok {
			return nil, errors.Errorf("invalid target options of type %T", opts.TargetOptions)
		}
		return nodejs.New(projectName, opts.TargetSDKVersion, nodeOpts.UsePromptDataSources, opts.Writer)
	case LanguagePython:
		return python.New(projectName, opts.Writer), nil
	default:
		return nil, errors.Errorf("invalid language '%s', expected one of %s",
			opts.TargetLanguage, strings.Join(ValidLanguages[:], ", "))
	}
}
