package steps

import (
	"fmt"

	appsclientset "github.com/openshift/client-go/apps/clientset/versioned/typed/apps/v1"
	buildclientset "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	imageclientset "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	routeclientset "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/openshift/ci-operator/pkg/api"
)

const (
	// PipelineImageStream is the name of the
	// ImageStream used to hold images built
	// to cache build steps in the pipeline.
	PipelineImageStream = "pipeline"

	// DefaultRPMLocation is the default relative
	// directory for Origin-based projects to put
	// their built RPMs.
	DefaultRPMLocation = "_output/local/releases/rpms/"

	// RPMServeLocation is the location from which
	// we will serve RPMs after they are built.
	RPMServeLocation = "/srv/repo"

	// StableImageNamespace is the default namespace
	// that holds stable published images as parts of
	// a full release.
	StableImageNamespace = "stable"
)

// FromConfig interprets the human-friendly fields in
// the release build configuration and generates steps for
// them, returning the full set of steps requires for the
// build, including defaulted steps, generated steps and
// all raw steps that the user provided.
func FromConfig(config *api.ReleaseBuildConfiguration, jobSpec *JobSpec, clusterConfig *rest.Config) ([]api.Step, error) {
	var buildSteps []api.Step

	jobNamespace := jobSpec.Identifier()

	buildGetter, err := buildclientset.NewForConfig(clusterConfig)
	if err != nil {
		return buildSteps, fmt.Errorf("could not get Build client for cluster config: %v", err)
	}
	buildClient := buildGetter.Builds(jobNamespace)

	imageGetter, err := imageclientset.NewForConfig(clusterConfig)
	if err != nil {
		return buildSteps, fmt.Errorf("could not get Image client for cluster config: %v", err)
	}
	imageStreamTagClient := imageGetter.ImageStreamTags(jobNamespace)

	routeGetter, err := routeclientset.NewForConfig(clusterConfig)
	if err != nil {
		return buildSteps, fmt.Errorf("could not get Route client for cluster config: %v", err)
	}
	routeClient := routeGetter.Routes(jobNamespace)

	appsGetter, err := appsclientset.NewForConfig(clusterConfig)
	if err != nil {
		return buildSteps, fmt.Errorf("could not get DeploymentConfig client for cluster config: %v", err)
	}
	deploymentClient := appsGetter.DeploymentConfigs(jobNamespace)

	coreGetter, err := coreclientset.NewForConfig(clusterConfig)
	if err != nil {
		return buildSteps, fmt.Errorf("could not get Service client for cluster config: %v", err)
	}
	serviceClient := coreGetter.Services(jobNamespace)

	for _, rawStep := range stepConfigsForBuild(config, jobSpec) {
		var step api.Step
		if rawStep.ImageTagStepConfiguration != nil {
			step = ImageTagStep(*rawStep.ImageTagStepConfiguration, imageStreamTagClient, jobSpec)
		} else if rawStep.PipelineImageCacheStepConfiguration != nil {
			step = PipelineImageCacheStep(*rawStep.PipelineImageCacheStepConfiguration, buildClient, imageStreamTagClient, jobSpec)
		} else if rawStep.SourceStepConfiguration != nil {
			step = SourceStep(*rawStep.SourceStepConfiguration, buildClient, imageStreamTagClient, jobSpec)
		} else if rawStep.ProjectDirectoryImageBuildStepConfiguration != nil {
			step = ProjectDirectoryImageBuildStep(*rawStep.ProjectDirectoryImageBuildStepConfiguration, buildClient, imageStreamTagClient, jobSpec)
		} else if rawStep.RPMImageInjectionStepConfiguration != nil {
			step = RPMImageInjectionStep(*rawStep.RPMImageInjectionStepConfiguration, buildClient, routeClient, imageStreamTagClient, jobSpec)
		} else if rawStep.RPMServeStepConfiguration != nil {
			step = RPMServerStep(*rawStep.RPMServeStepConfiguration, deploymentClient, routeClient, serviceClient, imageStreamTagClient, jobSpec)
		}

		buildSteps = append(buildSteps, step)
	}

	return buildSteps, nil
}

func stepConfigsForBuild(config *api.ReleaseBuildConfiguration, jobSpec *JobSpec) []api.StepConfiguration {
	var buildSteps []api.StepConfiguration

	if config.TestBaseImage != nil {
		if config.TestBaseImage.Namespace == "" {
			config.TestBaseImage.Namespace = StableImageNamespace
		}
		if config.TestBaseImage.Name == "" {
			config.TestBaseImage.Name = fmt.Sprintf("%s-test-base", jobSpec.Refs.Repo)
		}
		buildSteps = append(buildSteps, api.StepConfiguration{ImageTagStepConfiguration:
		&api.ImageTagStepConfiguration{
			BaseImage: *config.TestBaseImage,
			To:        api.PipelineImageStreamTagReferenceBase,
		}})
	}

	buildSteps = append(buildSteps, api.StepConfiguration{SourceStepConfiguration:
	&api.SourceStepConfiguration{
		From: api.PipelineImageStreamTagReferenceBase,
		To:   api.PipelineImageStreamTagReferenceSource,
	}})

	if len(config.BinaryBuildCommands) > 0 {
		buildSteps = append(buildSteps, api.StepConfiguration{PipelineImageCacheStepConfiguration:
		&api.PipelineImageCacheStepConfiguration{
			From:     api.PipelineImageStreamTagReferenceSource,
			To:       api.PipelineImageStreamTagReferenceBinaries,
			Commands: config.BinaryBuildCommands,
		}})
	}

	if len(config.TestBinaryBuildCommands) > 0 {
		buildSteps = append(buildSteps, api.StepConfiguration{PipelineImageCacheStepConfiguration:
		&api.PipelineImageCacheStepConfiguration{
			From:     api.PipelineImageStreamTagReferenceSource,
			To:       api.PipelineImageStreamTagReferenceTestBinaries,
			Commands: config.TestBinaryBuildCommands,
		}})
	}

	if len(config.RpmBuildCommands) > 0 {
		var from api.PipelineImageStreamTagReference
		if len(config.BinaryBuildCommands) > 0 {
			from = api.PipelineImageStreamTagReferenceBinaries
		} else {
			from = api.PipelineImageStreamTagReferenceSource
		}

		var out string
		if config.RpmBuildLocation != "" {
			out = config.RpmBuildLocation
		} else {
			out = DefaultRPMLocation
		}

		buildSteps = append(buildSteps, api.StepConfiguration{PipelineImageCacheStepConfiguration:
		&api.PipelineImageCacheStepConfiguration{
			From:     from,
			To:       api.PipelineImageStreamTagReferenceRPMs,
			Commands: fmt.Sprintf(`%s; ln -s $( pwd )/%s %s`, config.RpmBuildCommands, out, RPMServeLocation),
		}})

		buildSteps = append(buildSteps, api.StepConfiguration{RPMServeStepConfiguration:
		&api.RPMServeStepConfiguration{
			From: api.PipelineImageStreamTagReferenceRPMs,
		}})
	}

	for _, baseImage := range config.BaseImages {
		buildSteps = append(buildSteps, api.StepConfiguration{ImageTagStepConfiguration:
		&api.ImageTagStepConfiguration{
			BaseImage: baseImage,
			To:        api.PipelineImageStreamTagReference(baseImage.Name),
		}})
	}

	for _, baseRPMImage := range config.BaseRPMImages {
		intermediateTag := api.PipelineImageStreamTagReference(fmt.Sprintf("%s-without-rpms", baseRPMImage.Name))
		buildSteps = append(buildSteps, api.StepConfiguration{ImageTagStepConfiguration:
		&api.ImageTagStepConfiguration{
			BaseImage: baseRPMImage,
			To:        intermediateTag,
		}})

		buildSteps = append(buildSteps, api.StepConfiguration{RPMImageInjectionStepConfiguration:
		&api.RPMImageInjectionStepConfiguration{
			From: intermediateTag,
			To:   api.PipelineImageStreamTagReference(baseRPMImage.Name),
		}})
	}

	for _, image := range config.Images {
		buildSteps = append(buildSteps, api.StepConfiguration{ProjectDirectoryImageBuildStepConfiguration: &image})
	}

	buildSteps = append(buildSteps, config.RawSteps...)

	return buildSteps
}