// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

// Helm chart generator
//
// Fetches the given chart from {ChartRepo}/{ChartName},
// and inflates it to stdout, using the given values file.
// This generator expects helm V3 or later.

//go:generate pluginator
package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/imdario/mergo"
	"github.com/pkg/errors"
	"sigs.k8s.io/kustomize/api/filesys"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"
)

// HelmChartInflationGeneratorPlugin is a plugin to generate resources
// from a remote or local helm chart.
type HelmChartInflationGeneratorPlugin struct {
	h                *resmap.PluginHelpers
	types.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	runHelmCommand   func([]string) ([]byte, error)
	types.HelmChartArgs
	tmpDir string
}

//noinspection GoUnusedGlobalVariable
var KustomizePlugin HelmChartInflationGeneratorPlugin

// Config uses the input plugin configurations `config` to setup the generator
// options
func (p *HelmChartInflationGeneratorPlugin) Config(h *resmap.PluginHelpers, config []byte) error {
	p.h = h
	err := yaml.Unmarshal(config, p)
	if err != nil {
		return err
	}
	tmpDir, err := filesys.NewTmpConfirmedDir()
	if err != nil {
		return err
	}
	p.tmpDir = string(tmpDir)
	if p.ChartName == "" {
		return fmt.Errorf("chartName cannot be empty")
	}
	if p.ChartHome == "" {
		p.ChartHome = filepath.Join(p.tmpDir, "chart")
	}
	if p.ChartRepoName == "" {
		p.ChartRepoName = "stable"
	}
	if p.HelmBin == "" {
		p.HelmBin = "helm"
	}
	if p.HelmHome == "" {
		p.HelmHome = filepath.Join(p.tmpDir, ".helm")
	}
	if p.Values == "" {
		p.Values = filepath.Join(p.ChartHome, p.ChartName, "values.yaml")
	}
	if p.ValuesMerge == "" {
		p.ValuesMerge = "override"
	}
	// runHelmCommand will run `helm` command with args provided. Return stdout
	// and error if there is any.
	p.runHelmCommand = func(args []string) ([]byte, error) {
		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		cmd := exec.Command(p.HelmBin, args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.Env = append(os.Environ(),
			fmt.Sprintf("HELM_CONFIG_HOME=%s", p.HelmHome),
			fmt.Sprintf("HELM_CACHE_HOME=%s/.cache", p.HelmHome),
			fmt.Sprintf("HELM_DATA_HOME=%s/.data", p.HelmHome),
		)
		err := cmd.Run()
		if err != nil {
			return stdout.Bytes(),
				errors.Wrap(
					fmt.Errorf("failed to run command %s %s", p.HelmBin, strings.Join(args, " ")),
					stderr.String(),
				)
		}
		return stdout.Bytes(), nil
	}
	return nil
}

// EncodeValues for writing
func (p *HelmChartInflationGeneratorPlugin) EncodeValues(w io.Writer) error {
	d, err := yaml.Marshal(p.ValuesLocal)
	if err != nil {
		return err
	}
	_, err = w.Write(d)
	if err != nil {
		return err
	}
	return nil
}

// useValuesLocal process (merge) inflator config provided values with chart default values.yaml
func (p *HelmChartInflationGeneratorPlugin) useValuesLocal() error {
	// not override, merge, none
	if !(p.ValuesMerge == "none" || p.ValuesMerge == "no" || p.ValuesMerge == "false") {
		var pValues []byte
		var err error

		if filepath.IsAbs(p.Values) {
			pValues, err = ioutil.ReadFile(p.Values)
		} else {
			pValues, err = p.h.Loader().Load(p.Values)
		}
		if err != nil {
			return err
		}
		chValues := make(map[string]interface{})
		err = yaml.Unmarshal(pValues, &chValues)
		if err != nil {
			return err
		}
		if p.ValuesMerge == "override" {
			err = mergo.Merge(&chValues, p.ValuesLocal, mergo.WithOverride)
			if err != nil {
				return err
			}
		}
		if p.ValuesMerge == "merge" {
			err = mergo.Merge(&chValues, p.ValuesLocal)
			if err != nil {
				return err
			}
		}
		p.ValuesLocal = chValues
	}
	b, err := yaml.Marshal(p.ValuesLocal)
	if err != nil {
		return err
	}
	path, err := p.writeValuesBytes(b)
	if err != nil {
		return err
	}
	p.Values = path
	return nil
}

// copyValues will copy the relative values file into the temp directory
// to avoid messing up with CWD.
func (p *HelmChartInflationGeneratorPlugin) copyValues() error {
	// only copy when the values path is not absolute
	if filepath.IsAbs(p.Values) {
		return nil
	}
	// we must use use loader to read values file
	b, err := p.h.Loader().Load(p.Values)
	if err != nil {
		return err
	}
	path, err := p.writeValuesBytes(b)
	if err != nil {
		return err
	}
	p.Values = path
	return nil
}

func (p *HelmChartInflationGeneratorPlugin) writeValuesBytes(b []byte) (string, error) {
	path := filepath.Join(p.ChartHome, p.ChartName, "kustomize-values.yaml")
	err := ioutil.WriteFile(path, b, 0644)
	if err != nil {
		return "", err
	}
	return path, nil
}

// Generate implements generator
func (p *HelmChartInflationGeneratorPlugin) Generate() (resmap.ResMap, error) {
	// cleanup
	defer os.RemoveAll(p.tmpDir)
	// check helm version. we only support V3
	err := p.checkHelmVersion()
	if err != nil {
		return nil, err
	}
	// pull the chart
	if !p.checkLocalChart() {
		_, err := p.runHelmCommand(p.getPullCommandArgs())
		if err != nil {
			return nil, err
		}
	}

	// inflator config valuesLocal
	if len(p.ValuesLocal) > 0 {
		err := p.useValuesLocal()
		if err != nil {
			return nil, err
		}
	} else {
		err := p.copyValues()
		if err != nil {
			return nil, err
		}
	}

	// render the charts
	stdout, err := p.runHelmCommand(p.getTemplateCommandArgs())
	if err != nil {
		return nil, err
	}

	return p.h.ResmapFactory().NewResMapFromBytes(stdout)
}

func (p *HelmChartInflationGeneratorPlugin) getTemplateCommandArgs() []string {
	args := []string{"template"}
	if p.ReleaseName != "" {
		args = append(args, p.ReleaseName)
	}
	args = append(args, filepath.Join(p.ChartHome, p.ChartName))
	if p.ReleaseNamespace != "" {
		args = append(args, "--namespace", p.ReleaseNamespace)
	}
	if p.Values != "" {
		args = append(args, "--values", p.Values)
	}
	args = append(args, p.ExtraArgs...)
	return args
}

func (p *HelmChartInflationGeneratorPlugin) getPullCommandArgs() []string {
	args := []string{"pull", "--untar", "--untardir", p.ChartHome}
	chartName := fmt.Sprintf("%s/%s", p.ChartRepoName, p.ChartName)
	if p.ChartVersion != "" {
		args = append(args, "--version", p.ChartVersion)
	}
	if p.ChartRepoURL != "" {
		args = append(args, "--repo", p.ChartRepoURL)
		chartName = p.ChartName
	}

	args = append(args, chartName)

	return args
}

// checkLocalChart will return true if the chart does exist in
// local chart home.
func (p *HelmChartInflationGeneratorPlugin) checkLocalChart() bool {
	path := filepath.Join(p.ChartHome, p.ChartName)
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	return s.IsDir()
}

// checkHelmVersion will return an error if the helm version is not V3
func (p *HelmChartInflationGeneratorPlugin) checkHelmVersion() error {
	stdout, err := p.runHelmCommand([]string{"version", "-c", "--short"})
	if err != nil {
		return err
	}
	r, err := regexp.Compile(`v?\d+(\.\d+)+`)
	if err != nil {
		return err
	}
	v := r.FindString(string(stdout))
	if v == "" {
		return fmt.Errorf("cannot find version string in %s", string(stdout))
	}
	if v[0] == 'v' {
		v = v[1:]
	}
	majorVersion := strings.Split(v, ".")[0]
	if majorVersion != "3" {
		return fmt.Errorf("this plugin requires helm V3 but got v%s", v)
	}
	return nil
}
