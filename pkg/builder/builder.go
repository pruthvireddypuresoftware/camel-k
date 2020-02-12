/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package builder

import (
	"io/ioutil"
	"os"
	"path"
	"sort"
	"time"

	v1 "github.com/apache/camel-k/pkg/apis/camel/v1"
	"github.com/apache/camel-k/pkg/client"
	"github.com/apache/camel-k/pkg/util/cancellable"
	"github.com/apache/camel-k/pkg/util/log"
)

type defaultBuilder struct {
	log    log.Logger
	ctx    cancellable.Context
	client client.Client
}

// New --
func New(c client.Client) Builder {
	m := defaultBuilder{
		log:    log.WithName("builder"),
		ctx:    cancellable.NewContext(),
		client: c,
	}

	return &m
}

// Run --
func (b *defaultBuilder) Run(build v1.BuilderTask) v1.BuildStatus {
	result := v1.BuildStatus{}

	var buildDir string
	if build.BuildDir == "" {
		tmpDir, err := ioutil.TempDir(os.TempDir(), "builder-")
		if err != nil {
			log.Error(err, "Unexpected error while creating a temporary dir")

			result.Phase = v1.BuildPhaseFailed
			result.Error = err.Error()
		}
		buildDir = tmpDir
		defer os.RemoveAll(buildDir)
	} else {
		buildDir = build.BuildDir
	}

	c := Context{
		Client:    b.client,
		Path:      buildDir,
		Namespace: build.Meta.Namespace,
		Build:     build,
		BaseImage: build.BaseImage,
	}

	if build.Image != "" {
		c.BaseImage = build.Image
	}

	// base image is mandatory
	if c.BaseImage == "" {
		result.Phase = v1.BuildPhaseFailed
		result.Image = ""
		result.Error = "no base image defined"
	}

	// Add sources
	for _, data := range build.Sources {
		c.Resources = append(c.Resources, Resource{
			Content: []byte(data.Content),
			Target:  path.Join("sources", data.Name),
		})
	}

	// Add resources
	for _, data := range build.Resources {
		t := path.Join("resources", data.Name)

		if data.MountPath != "" {
			t = path.Join(data.MountPath, data.Name)
		}

		c.Resources = append(c.Resources, Resource{
			Content: []byte(data.Content),
			Target:  t,
		})
	}

	if result.Phase == v1.BuildPhaseFailed {
		return result
	}

	steps := make([]Step, 0)
	for _, step := range build.Steps {
		s, ok := stepsByID[step]
		if !ok {
			log.Info("Skipping unknown build step", "step", step)
			continue
		}
		steps = append(steps, s)
	}
	// Sort steps by phase
	sort.SliceStable(steps, func(i, j int) bool {
		return steps[i].Phase() < steps[j].Phase()
	})

	b.log.Infof("steps: %v", steps)
	for _, step := range steps {
		if c.Error != nil || result.Phase == v1.BuildPhaseInterrupted {
			break
		}

		select {
		case <-b.ctx.Done():
			result.Phase = v1.BuildPhaseInterrupted
		default:
			l := b.log.WithValues(
				"step", step.ID(),
				"phase", step.Phase(),
				"name", build.Meta.Name,
				"task", build.Name,
			)

			l.Infof("executing step")

			start := time.Now()
			c.Error = step.Execute(&c)

			if c.Error == nil {
				l.Infof("step done in %f seconds", time.Since(start).Seconds())
			} else {
				l.Infof("step failed with error: %s", c.Error)
			}
		}
	}

	if result.Phase != v1.BuildPhaseInterrupted {
		result.BaseImage = c.BaseImage
		result.Image = c.Image
		result.Digest = c.Digest

		if c.Error != nil {
			result.Error = c.Error.Error()
			result.Phase = v1.BuildPhaseFailed
		}

		result.Artifacts = make([]v1.Artifact, 0, len(c.Artifacts))
		result.Artifacts = append(result.Artifacts, c.Artifacts...)

		b.log.Infof("dependencies: %s", build.Dependencies)
		b.log.Infof("artifacts: %s", artifactIDs(c.Artifacts))
		b.log.Infof("artifacts selected: %s", artifactIDs(c.SelectedArtifacts))
		b.log.Infof("base image: %s", build.BaseImage)
		b.log.Infof("resolved base image: %s", c.BaseImage)
		b.log.Infof("resolved image: %s", c.Image)
	} else {
		b.log.Infof("build task %s interrupted", build.Name)
	}

	return result
}
