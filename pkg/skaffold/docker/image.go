/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/progress"
	"github.com/moby/moby/pkg/archive"
	"github.com/moby/moby/pkg/jsonmessage"
	"github.com/moby/moby/pkg/streamformatter"
	"github.com/moby/moby/pkg/term"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type BuildOptions struct {
	ImageName   string
	Dockerfile  string
	ContextDir  string
	ProgressBuf io.Writer
	BuildBuf    io.Writer
	BuildArgs   map[string]*string
}

// RunBuild performs a docker build and returns nothing
func RunBuild(ctx context.Context, cli DockerAPIClient, opts *BuildOptions) error {
	logrus.Debugf("Running docker build: context: %s, dockerfile: %s", opts.ContextDir, opts.Dockerfile)
	authConfigs, err := DefaultAuthHelper.GetAllAuthConfigs()
	if err != nil {
		return errors.Wrap(err, "read auth configs")
	}

	imageBuildOpts := types.ImageBuildOptions{
		Tags:        []string{opts.ImageName},
		Dockerfile:  opts.Dockerfile,
		BuildArgs:   opts.BuildArgs,
		AuthConfigs: authConfigs,
	}

	dockerfilePath := filepath.Join(opts.ContextDir, opts.Dockerfile)
	f, err := os.Open(dockerfilePath)
	if err != nil {
		return errors.Wrap(err, "opening dockerfile")
	}
	paths, err := GetDockerfileDependencies(opts.ContextDir, f)
	for i, path := range paths {
		paths[i] = strings.TrimPrefix(path, opts.ContextDir)
	}
	buildCtx, err := archive.TarWithOptions(opts.ContextDir, &archive.TarOptions{
		ChownOpts:    &idtools.IDPair{UID: 0, GID: 0},
		IncludeFiles: append(paths, opts.Dockerfile),
	})
	if err != nil {
		return errors.Wrap(err, "tar workspace")
	}

	progressOutput := streamformatter.NewProgressOutput(opts.ProgressBuf)
	body := progress.NewProgressReader(buildCtx, progressOutput, 0, "", "Sending build context to Docker daemon")

	resp, err := cli.ImageBuild(ctx, body, imageBuildOpts)
	if err != nil {
		return errors.Wrap(err, "docker build")
	}
	defer resp.Body.Close()
	return streamDockerMessages(opts.BuildBuf, resp.Body)
}

// TODO(@r2d4): Make this output much better, this is the bare minimum
func streamDockerMessages(dst io.Writer, src io.Reader) error {
	fd, _ := term.GetFdInfo(dst)
	return jsonmessage.DisplayJSONMessagesStream(src, dst, fd, false, nil)
}

func RunPush(ctx context.Context, cli DockerAPIClient, ref string, out io.Writer) error {
	registryAuth, err := encodedRegistryAuth(ctx, cli, DefaultAuthHelper, ref)
	if err != nil {
		return errors.Wrapf(err, "getting auth config for %s", ref)
	}
	rc, err := cli.ImagePush(ctx, ref, types.ImagePushOptions{
		RegistryAuth: registryAuth,
	})
	if err != nil {
		return errors.Wrap(err, "pushing image to repository")
	}
	defer rc.Close()
	return streamDockerMessages(out, rc)
}

// Digest returns the image digest for a corresponding reference.
// The digest is of the form
// sha256:<image_id>
func Digest(ctx context.Context, cli DockerAPIClient, ref string) (string, error) {
	refLatest := fmt.Sprintf("%s:latest", ref)
	args := filters.KeyValuePair{Key: "reference", Value: refLatest}
	filters := filters.NewArgs(args)
	imageList, err := cli.ImageList(ctx, types.ImageListOptions{
		Filters: filters,
	})
	if err != nil {
		return "", errors.Wrap(err, "getting image id")
	}
	for _, image := range imageList {
		for _, tag := range image.RepoTags {
			if tag == refLatest {
				return image.ID, nil
			}
		}
	}
	return "", nil
}
