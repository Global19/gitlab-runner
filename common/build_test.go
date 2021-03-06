package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"gitlab.com/gitlab-org/gitlab-runner/helpers/docker"
	"gitlab.com/gitlab-org/gitlab-runner/helpers/featureflags"
	"gitlab.com/gitlab-org/gitlab-runner/session"
	"gitlab.com/gitlab-org/gitlab-runner/session/terminal"
)

func init() {
	s := MockShell{}
	s.On("GetName").Return("script-shell")
	s.On("GenerateScript", mock.Anything, mock.Anything).Return("script", nil)
	RegisterShell(&s)
}

func TestBuildPredefinedVariables(t *testing.T) {
	for _, rootDir := range []string{"/root/dir1", "/root/dir2"} {
		t.Run(rootDir, func(t *testing.T) {
			build := runSuccessfulMockBuild(t, func(options ExecutorPrepareOptions) error {
				return options.Build.StartBuild(rootDir, "/cache/dir", false, false)
			})

			projectDir := build.GetAllVariables().Get("CI_PROJECT_DIR")
			assert.NotEmpty(t, projectDir, "should have CI_PROJECT_DIR")
		})
	}
}

func matchBuildStage(buildStage BuildStage) interface{} {
	return mock.MatchedBy(func(cmd ExecutorCommand) bool {
		return cmd.Stage == buildStage
	})
}

func TestBuildRun(t *testing.T) {
	runSuccessfulMockBuild(t, func(options ExecutorPrepareOptions) error { return nil })
}

func TestJobImageExposed(t *testing.T) {
	tests := map[string]struct {
		image           string
		vars            []JobVariable
		expectVarExists bool
		expectImageName string
	}{
		"normal image exposed": {
			image:           "alpine:3.11",
			expectVarExists: true,
			expectImageName: "alpine:3.11",
		},
		"image with variable expansion": {
			image:           "${IMAGE}:3.11",
			vars:            []JobVariable{{Key: "IMAGE", Value: "alpine", Public: true}},
			expectVarExists: true,
			expectImageName: "alpine:3.11",
		},
		"no image specified": {
			image:           "",
			expectVarExists: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			build := runSuccessfulMockBuild(t, func(options ExecutorPrepareOptions) error {
				options.Build.Image.Name = tt.image
				options.Build.Variables = append(options.Build.Variables, tt.vars...)
				return options.Build.StartBuild("/root/dir", "/cache/dir", false, false)
			})

			actualVarExists := false
			for _, v := range build.GetAllVariables() {
				if v.Key == "CI_JOB_IMAGE" {
					actualVarExists = true
					break
				}
			}
			assert.Equal(t, tt.expectVarExists, actualVarExists, "CI_JOB_IMAGE exported?")

			if tt.expectVarExists {
				actualJobImage := build.GetAllVariables().Get("CI_JOB_IMAGE")
				assert.Equal(t, tt.expectImageName, actualJobImage)
			}
		})
	}
}

func TestBuildRunNoModifyConfig(t *testing.T) {
	expectHostAddr := "10.0.0.1"
	p, assertFn := setupSuccessfulMockExecutor(t, func(options ExecutorPrepareOptions) error {
		options.Config.Docker.Credentials.Host = "10.0.0.2"
		return nil
	})
	defer assertFn()

	rc := &RunnerConfig{
		RunnerSettings: RunnerSettings{
			Docker: &DockerConfig{
				Credentials: docker.Credentials{
					Host: expectHostAddr,
				},
			},
		},
	}
	build := registerExecutorWithSuccessfulBuild(t, p, rc)

	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.NoError(t, err)
	assert.Equal(t, expectHostAddr, rc.Docker.Credentials.Host)
}

func TestRetryPrepare(t *testing.T) {
	PreparationRetryInterval = 0

	e := MockExecutor{}
	defer e.AssertExpectations(t)

	p := MockExecutorProvider{}
	defer p.AssertExpectations(t)

	// Create executor
	p.On("CanCreate").Return(true).Once()
	p.On("GetDefaultShell").Return("bash").Once()
	p.On("GetFeatures", mock.Anything).Return(nil).Twice()

	p.On("Create").Return(&e).Times(3)

	// Prepare plan
	e.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("prepare failed")).Twice()
	e.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	e.On("Cleanup").Times(3)

	// Succeed a build script
	e.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	e.On("Run", mock.Anything).Return(nil)
	e.On("Finish", nil).Once()

	build := registerExecutorWithSuccessfulBuild(t, &p, new(RunnerConfig))
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.NoError(t, err)
}

func TestPrepareFailure(t *testing.T) {
	PreparationRetryInterval = 0

	e := MockExecutor{}
	defer e.AssertExpectations(t)

	p := MockExecutorProvider{}
	defer p.AssertExpectations(t)

	// Create executor
	p.On("CanCreate").Return(true).Once()
	p.On("GetDefaultShell").Return("bash").Once()
	p.On("GetFeatures", mock.Anything).Return(nil).Twice()

	p.On("Create").Return(&e).Times(3)

	// Prepare plan
	e.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("prepare failed")).Times(3)
	e.On("Cleanup").Times(3)

	build := registerExecutorWithSuccessfulBuild(t, &p, new(RunnerConfig))
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.EqualError(t, err, "prepare failed")
}

func TestPrepareFailureOnBuildError(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildError{}).Once()
	executor.On("Cleanup").Once()

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})

	expectedErr := new(BuildError)
	assert.True(t, errors.Is(err, expectedErr), "expected: %#v, got: %#v", expectedErr, err)
}

func TestPrepareEnvironmentFailure(t *testing.T) {
	testErr := errors.New("test-err")

	e := new(MockExecutor)
	defer e.AssertExpectations(t)

	p := new(MockExecutorProvider)
	defer p.AssertExpectations(t)

	p.On("CanCreate").Return(true).Once()
	p.On("GetDefaultShell").Return("bash").Once()
	p.On("GetFeatures", mock.Anything).Return(nil).Twice()
	p.On("Create").Return(e).Once()

	e.On("Prepare", mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	e.On("Cleanup").Once()
	e.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	e.On("Run", matchBuildStage(BuildStagePrepare)).Return(testErr).Once()
	e.On("Finish", mock.Anything).Once()

	RegisterExecutorProvider("build-run-prepare-environment-failure-on-build-error", p)

	successfulBuild, err := GetSuccessfulBuild()
	assert.NoError(t, err)
	build := &Build{
		JobResponse: successfulBuild,
		Runner: &RunnerConfig{
			RunnerSettings: RunnerSettings{
				Executor: "build-run-prepare-environment-failure-on-build-error",
			},
		},
	}

	err = build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.True(t, errors.Is(err, testErr))
}

func TestJobFailure(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Set up a failing a build script
	thrownErr := &BuildError{Inner: errors.New("test error")}
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", matchBuildStage(BuildStagePrepare)).Return(nil).Once()
	executor.On("Run", mock.Anything).Return(thrownErr).Times(2)
	executor.On("Finish", thrownErr).Once()

	RegisterExecutorProvider("build-run-job-failure", provider)

	failedBuild, err := GetFailedBuild()
	assert.NoError(t, err)
	build := &Build{
		JobResponse: failedBuild,
		Runner: &RunnerConfig{
			RunnerSettings: RunnerSettings{
				Executor: "build-run-job-failure",
			},
		},
	}

	trace := new(MockJobTrace)
	defer trace.AssertExpectations(t)
	trace.On("Write", mock.Anything).Return(0, nil)
	trace.On("IsStdout").Return(true)
	trace.On("SetCancelFunc", mock.Anything).Once()
	trace.On("SetMasked", mock.Anything).Once()
	trace.On("Fail", thrownErr, ScriptFailure).Once()

	err = build.Run(&Config{}, trace)

	expectedErr := new(BuildError)
	assert.True(t, errors.Is(err, expectedErr), "expected: %#v, got: %#v", expectedErr, err)
}

func TestJobFailureOnExecutionTimeout(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)

	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Succeed a build script
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", matchBuildStage("step_script")).Run(func(mock.Arguments) {
		time.Sleep(2 * time.Second)
	}).Return(nil)
	executor.On("Run", mock.Anything).Return(nil)
	executor.On("Finish", mock.Anything).Once()

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	build.JobResponse.RunnerInfo.Timeout = 1

	trace := new(MockJobTrace)
	defer trace.AssertExpectations(t)
	trace.On("Write", mock.Anything).Return(0, nil)
	trace.On("IsStdout").Return(true)
	trace.On("SetCancelFunc", mock.Anything).Once()
	trace.On("SetMasked", mock.Anything).Once()
	trace.On("Fail", mock.Anything, JobExecutionTimeout).Run(func(arguments mock.Arguments) {
		assert.Error(t, arguments.Get(0).(error))
	}).Once()

	err := build.Run(&Config{}, trace)

	expectedErr := &BuildError{FailureReason: JobExecutionTimeout}
	assert.True(t, errors.Is(err, expectedErr), "expected: %#v, got: %#v", expectedErr, err)
}

func TestRunFailureRunsAfterScriptAndArtifactsOnFailure(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Fail a build script
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", matchBuildStage(BuildStagePrepare)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageGetSources)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageRestoreCache)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageDownloadArtifacts)).Return(nil).Once()
	executor.On("Run", matchBuildStage("step_script")).Return(errors.New("build fail")).Once()
	executor.On("Run", matchBuildStage(BuildStageAfterScript)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageUploadOnFailureArtifacts)).Return(nil).Once()
	executor.On("Finish", errors.New("build fail")).Once()

	RegisterExecutorProvider("build-run-run-failure", provider)

	failedBuild, err := GetFailedBuild()
	assert.NoError(t, err)
	build := &Build{
		JobResponse: failedBuild,
		Runner: &RunnerConfig{
			RunnerSettings: RunnerSettings{
				Executor: "build-run-run-failure",
			},
		},
	}
	err = build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.EqualError(t, err, "build fail")
}

func TestGetSourcesRunFailure(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Fail a build script
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", matchBuildStage(BuildStagePrepare)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageGetSources)).Return(errors.New("build fail")).Times(3)
	executor.On("Run", matchBuildStage(BuildStageUploadOnFailureArtifacts)).Return(nil).Once()
	executor.On("Finish", errors.New("build fail")).Once()

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	build.Variables = append(build.Variables, JobVariable{Key: "GET_SOURCES_ATTEMPTS", Value: "3"})
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.EqualError(t, err, "build fail")
}

func TestArtifactDownloadRunFailure(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Fail a build script
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", matchBuildStage(BuildStagePrepare)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageGetSources)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageRestoreCache)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageDownloadArtifacts)).Return(errors.New("build fail")).Times(3)
	executor.On("Run", matchBuildStage(BuildStageUploadOnFailureArtifacts)).Return(nil).Once()
	executor.On("Finish", errors.New("build fail")).Once()

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	build.Variables = append(build.Variables, JobVariable{Key: "ARTIFACT_DOWNLOAD_ATTEMPTS", Value: "3"})
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.EqualError(t, err, "build fail")
}

func TestArtifactUploadRunFailure(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Successful build script
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"}).Times(8)
	executor.On("Run", matchBuildStage(BuildStagePrepare)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageGetSources)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageRestoreCache)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageDownloadArtifacts)).Return(nil).Once()
	executor.On("Run", matchBuildStage("step_script")).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageAfterScript)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageArchiveCache)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageUploadOnSuccessArtifacts)).Return(errors.New("upload fail")).Once()
	executor.On("Finish", errors.New("upload fail")).Once()

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	successfulBuild := build.JobResponse
	successfulBuild.Artifacts = make(Artifacts, 1)
	successfulBuild.Artifacts[0] = Artifact{
		Name:      "my-artifact",
		Untracked: false,
		Paths:     ArtifactPaths{"cached/*"},
		When:      ArtifactWhenAlways,
	}
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.EqualError(t, err, "upload fail")
}

func TestRestoreCacheRunFailure(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer executor.AssertExpectations(t)
	defer provider.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Fail a build script
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", matchBuildStage(BuildStagePrepare)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageGetSources)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageRestoreCache)).Return(errors.New("build fail")).Times(3)
	executor.On("Run", matchBuildStage(BuildStageUploadOnFailureArtifacts)).Return(nil).Once()
	executor.On("Finish", errors.New("build fail")).Once()

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	build.Variables = append(build.Variables, JobVariable{Key: "RESTORE_CACHE_ATTEMPTS", Value: "3"})
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.EqualError(t, err, "build fail")
}

func TestRunWrongAttempts(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer provider.AssertExpectations(t)
	defer executor.AssertExpectations(t)
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()
	executor.On("Cleanup").Once()

	// Fail a build script
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", mock.Anything).Return(nil).Once()
	executor.
		On("Run", mock.Anything).
		Return(errors.New("number of attempts out of the range [1, 10] for stage: get_sources"))
	executor.On(
		"Finish",
		errors.New("number of attempts out of the range [1, 10] for stage: get_sources"),
	)

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	build.Variables = append(build.Variables, JobVariable{Key: "GET_SOURCES_ATTEMPTS", Value: "0"})
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.EqualError(t, err, "number of attempts out of the range [1, 10] for stage: get_sources")
}

func TestRunSuccessOnSecondAttempt(t *testing.T) {
	executor, provider := setupMockExecutorAndProvider()
	defer provider.AssertExpectations(t)

	// We run everything once
	executor.On("Prepare", mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	executor.On("Finish", mock.Anything).Twice()
	executor.On("Cleanup").Twice()

	// Run script successfully
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})

	executor.On("Run", mock.Anything).Return(nil)
	executor.On("Run", mock.Anything).Return(errors.New("build fail")).Once()
	executor.On("Run", mock.Anything).Return(nil)

	build := registerExecutorWithSuccessfulBuild(t, provider, new(RunnerConfig))
	build.Variables = append(build.Variables, JobVariable{Key: "GET_SOURCES_ATTEMPTS", Value: "3"})
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.NoError(t, err)
}

func TestDebugTrace(t *testing.T) {
	testCases := map[string]struct {
		debugTraceVariableValue   string
		expectedValue             bool
		debugTraceFeatureDisabled bool
		expectedLogOutput         string
	}{
		"variable not set": {
			expectedValue: false,
		},
		"variable set to false": {
			debugTraceVariableValue: "false",
			expectedValue:           false,
		},
		"variable set to true": {
			debugTraceVariableValue: "true",
			expectedValue:           true,
		},
		"variable set to a non-bool value": {
			debugTraceVariableValue: "xyz",
			expectedValue:           false,
		},
		"variable set to true and feature disabled from configuration": {
			debugTraceVariableValue:   "true",
			expectedValue:             false,
			debugTraceFeatureDisabled: true,
			expectedLogOutput:         "CI_DEBUG_TRACE usage is disabled on this Runner",
		},
	}

	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			logger, hooks := test.NewNullLogger()

			build := &Build{
				logger: NewBuildLogger(nil, logrus.NewEntry(logger)),
				JobResponse: JobResponse{
					Variables: JobVariables{},
				},
				Runner: &RunnerConfig{
					RunnerSettings: RunnerSettings{
						DebugTraceDisabled: testCase.debugTraceFeatureDisabled,
					},
				},
			}

			if testCase.debugTraceVariableValue != "" {
				build.Variables = append(
					build.Variables,
					JobVariable{Key: "CI_DEBUG_TRACE", Value: testCase.debugTraceVariableValue, Public: true},
				)
			}

			isTraceEnabled := build.IsDebugTraceEnabled()
			assert.Equal(t, testCase.expectedValue, isTraceEnabled)

			if testCase.expectedLogOutput != "" {
				output, err := hooks.LastEntry().String()
				require.NoError(t, err)
				assert.Contains(t, output, testCase.expectedLogOutput)
			}
		})
	}
}

func TestDefaultEnvVariables(t *testing.T) {
	buildDir := "/tmp/test-build/dir"
	build := Build{
		BuildDir: buildDir,
	}

	vars := build.GetAllVariables().StringList()

	assert.Contains(t, vars, "CI_PROJECT_DIR="+filepath.FromSlash(buildDir))
	assert.Contains(t, vars, "CI_SERVER=yes")
}

func TestSharedEnvVariables(t *testing.T) {
	for _, shared := range [...]bool{true, false} {
		t.Run(fmt.Sprintf("Value:%v", shared), func(t *testing.T) {
			assert := assert.New(t)
			build := Build{
				ExecutorFeatures: FeaturesInfo{Shared: shared},
			}
			vars := build.GetAllVariables().StringList()

			assert.NotNil(vars)

			present := "CI_SHARED_ENVIRONMENT=true"
			absent := "CI_DISPOSABLE_ENVIRONMENT=true"
			if !shared {
				present, absent = absent, present
			}

			assert.Contains(vars, present)
			assert.NotContains(vars, absent)
			// we never expose false
			assert.NotContains(vars, "CI_SHARED_ENVIRONMENT=false")
			assert.NotContains(vars, "CI_DISPOSABLE_ENVIRONMENT=false")
		})
	}
}

func TestGetRemoteURL(t *testing.T) {
	testCases := []struct {
		runner RunnerSettings
		result string
	}{
		{
			runner: RunnerSettings{
				CloneURL: "http://test.local/",
			},
			result: "http://gitlab-ci-token:1234567@test.local/h5bp/html5-boilerplate.git",
		},
		{
			runner: RunnerSettings{
				CloneURL: "https://test.local",
			},
			result: "https://gitlab-ci-token:1234567@test.local/h5bp/html5-boilerplate.git",
		},
		{
			runner: RunnerSettings{},
			result: "http://fallback.url",
		},
	}

	for _, tc := range testCases {
		build := &Build{
			Runner: &RunnerConfig{
				RunnerSettings: tc.runner,
			},
			allVariables: JobVariables{
				JobVariable{Key: "CI_JOB_TOKEN", Value: "1234567"},
				JobVariable{Key: "CI_PROJECT_PATH", Value: "h5bp/html5-boilerplate"},
			},
			JobResponse: JobResponse{
				GitInfo: GitInfo{RepoURL: "http://fallback.url"},
			},
		}

		assert.Equal(t, tc.result, build.GetRemoteURL())
	}
}

type featureFlagOnTestCase struct {
	value          string
	expectedStatus bool
	expectedError  bool
}

func TestIsFeatureFlagOn(t *testing.T) {
	hook := test.NewGlobal()

	tests := map[string]featureFlagOnTestCase{
		"no value": {
			value:          "",
			expectedStatus: false,
			expectedError:  false,
		},
		"true": {
			value:          "true",
			expectedStatus: true,
			expectedError:  false,
		},
		"1": {
			value:          "1",
			expectedStatus: true,
			expectedError:  false,
		},
		"false": {
			value:          "false",
			expectedStatus: false,
			expectedError:  false,
		},
		"0": {
			value:          "0",
			expectedStatus: false,
			expectedError:  false,
		},
		"invalid value": {
			value:          "test",
			expectedStatus: false,
			expectedError:  true,
		},
	}

	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			build := new(Build)
			build.Variables = JobVariables{
				{Key: "FF_TEST_FEATURE", Value: testCase.value},
			}

			status := build.IsFeatureFlagOn("FF_TEST_FEATURE")
			assert.Equal(t, testCase.expectedStatus, status)

			entry := hook.LastEntry()
			if testCase.expectedError {
				require.NotNil(t, entry)

				logrusOutput, err := entry.String()
				require.NoError(t, err)

				assert.Contains(t, logrusOutput, "Error while parsing the value of feature flag")
			} else {
				assert.Nil(t, entry)
			}

			hook.Reset()
		})
	}
}

func TestAllowToOverwriteFeatureFlagWithRunnerVariables(t *testing.T) {
	tests := map[string]struct {
		variable      string
		expectedValue bool
	}{
		"it has default value of FF": {
			variable:      "",
			expectedValue: false,
		},
		"it enables FF": {
			variable:      "FF_NETWORK_PER_BUILD=true",
			expectedValue: true,
		},
		"it disable FF": {
			variable:      "FF_NETWORK_PER_BUILD=false",
			expectedValue: false,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			build := new(Build)
			build.Runner = &RunnerConfig{
				RunnerSettings: RunnerSettings{
					Environment: []string{test.variable},
				},
			}

			result := build.IsFeatureFlagOn("FF_NETWORK_PER_BUILD")
			assert.Equal(t, test.expectedValue, result)
		})
	}
}

func TestStartBuild(t *testing.T) {
	type startBuildArgs struct {
		rootDir               string
		cacheDir              string
		customBuildDirEnabled bool
		sharedDir             bool
	}

	tests := map[string]struct {
		args             startBuildArgs
		jobVariables     JobVariables
		expectedBuildDir string
		expectedCacheDir string
		expectedError    bool
	}{
		"no job specific build dir with no shared dir": {
			args: startBuildArgs{
				rootDir:               "/build",
				cacheDir:              "/cache",
				customBuildDirEnabled: true,
				sharedDir:             false,
			},
			jobVariables:     JobVariables{},
			expectedBuildDir: "/build/test-namespace/test-repo",
			expectedCacheDir: "/cache/test-namespace/test-repo",
			expectedError:    false,
		},
		"no job specified build dir with shared dir": {
			args: startBuildArgs{
				rootDir:               "/builds",
				cacheDir:              "/cache",
				customBuildDirEnabled: true,
				sharedDir:             true,
			},
			jobVariables:     JobVariables{},
			expectedBuildDir: "/builds/1234/0/test-namespace/test-repo",
			expectedCacheDir: "/cache/test-namespace/test-repo",
			expectedError:    false,
		},
		"valid GIT_CLONE_PATH was specified": {
			args: startBuildArgs{
				rootDir:               "/builds",
				cacheDir:              "/cache",
				customBuildDirEnabled: true,
				sharedDir:             false,
			},
			jobVariables: JobVariables{
				{Key: "GIT_CLONE_PATH", Value: "/builds/go/src/gitlab.com/test-namespace/test-repo", Public: true},
			},
			expectedBuildDir: "/builds/go/src/gitlab.com/test-namespace/test-repo",
			expectedCacheDir: "/cache/test-namespace/test-repo",
			expectedError:    false,
		},
		"valid GIT_CLONE_PATH using CI_BUILDS_DIR was specified": {
			args: startBuildArgs{
				rootDir:               "/builds",
				cacheDir:              "/cache",
				customBuildDirEnabled: true,
				sharedDir:             false,
			},
			jobVariables: JobVariables{
				{
					Key:    "GIT_CLONE_PATH",
					Value:  "$CI_BUILDS_DIR/go/src/gitlab.com/test-namespace/test-repo",
					Public: true,
				},
			},
			expectedBuildDir: "/builds/go/src/gitlab.com/test-namespace/test-repo",
			expectedCacheDir: "/cache/test-namespace/test-repo",
			expectedError:    false,
		},
		"custom build disabled": {
			args: startBuildArgs{
				rootDir:               "/builds",
				cacheDir:              "/cache",
				customBuildDirEnabled: false,
				sharedDir:             false,
			},
			jobVariables: JobVariables{
				{Key: "GIT_CLONE_PATH", Value: "/builds/go/src/gitlab.com/test-namespace/test-repo", Public: true},
			},
			expectedBuildDir: "/builds/test-namespace/test-repo",
			expectedCacheDir: "/cache/test-namespace/test-repo",
			expectedError:    true,
		},
		"invalid GIT_CLONE_PATH was specified": {
			args: startBuildArgs{
				rootDir:               "/builds",
				cacheDir:              "/cache",
				customBuildDirEnabled: true,
				sharedDir:             false,
			},
			jobVariables: JobVariables{
				{Key: "GIT_CLONE_PATH", Value: "/go/src/gitlab.com/test-namespace/test-repo", Public: true},
			},
			expectedError: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			build := Build{
				JobResponse: JobResponse{
					GitInfo: GitInfo{
						RepoURL: "https://gitlab.com/test-namespace/test-repo.git",
					},
					Variables: test.jobVariables,
				},
				Runner: &RunnerConfig{
					RunnerCredentials: RunnerCredentials{
						Token: "1234",
					},
				},
			}

			err := build.StartBuild(
				test.args.rootDir,
				test.args.cacheDir,
				test.args.customBuildDirEnabled,
				test.args.sharedDir,
			)
			if test.expectedError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, test.expectedBuildDir, build.BuildDir)
			assert.Equal(t, test.args.rootDir, build.RootDir)
			assert.Equal(t, test.expectedCacheDir, build.CacheDir)
		})
	}
}

func TestSkipBuildStageFeatureFlag(t *testing.T) {
	featureFlagValues := []string{
		"true",
		"false",
	}

	s := MockShell{}
	s.On("GetName").Return("skip-build-stage-shell")
	RegisterShell(&s)

	for _, value := range featureFlagValues {
		t.Run(value, func(t *testing.T) {
			build := &Build{
				Runner: &RunnerConfig{},
				JobResponse: JobResponse{
					Variables: JobVariables{
						{
							Key:   featureflags.SkipNoOpBuildStages,
							Value: "false",
						},
					},
				},
			}

			e := &MockExecutor{}
			defer e.AssertExpectations(t)

			s.On("GenerateScript", mock.Anything, mock.Anything).Return("script", ErrSkipBuildStage)
			e.On("Shell").Return(&ShellScriptInfo{Shell: "skip-build-stage-shell"})

			if !build.IsFeatureFlagOn(featureflags.SkipNoOpBuildStages) {
				e.On("Run", matchBuildStage(BuildStageAfterScript)).Return(nil).Once()
			}

			err := build.executeStage(context.Background(), BuildStageAfterScript, e)
			assert.NoError(t, err)
		})
	}
}

func TestWaitForTerminal(t *testing.T) {
	cases := []struct {
		name                   string
		cancelFn               func(ctxCancel context.CancelFunc, build *Build)
		jobTimeout             int
		waitForTerminalTimeout time.Duration
		expectedErr            string
	}{
		{
			name: "Cancel build",
			cancelFn: func(ctxCancel context.CancelFunc, build *Build) {
				ctxCancel()
			},
			jobTimeout:             3600,
			waitForTerminalTimeout: time.Hour,
			expectedErr:            "build cancelled, killing session",
		},
		{
			name: "Terminal Timeout",
			cancelFn: func(ctxCancel context.CancelFunc, build *Build) {
				// noop
			},
			jobTimeout:             3600,
			waitForTerminalTimeout: time.Second,
			expectedErr:            "terminal session timed out (maximum time allowed - 1s)",
		},
		{
			name: "System Interrupt",
			cancelFn: func(ctxCancel context.CancelFunc, build *Build) {
				build.SystemInterrupt <- os.Interrupt
			},
			jobTimeout:             3600,
			waitForTerminalTimeout: time.Hour,
			expectedErr:            "terminal disconnected by system signal: interrupt",
		},
		{
			name: "Terminal Disconnect",
			cancelFn: func(ctxCancel context.CancelFunc, build *Build) {
				build.Session.DisconnectCh <- errors.New("user disconnect")
			},
			jobTimeout:             3600,
			waitForTerminalTimeout: time.Hour,
			expectedErr:            "terminal disconnected: user disconnect",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			build := Build{
				Runner: &RunnerConfig{
					RunnerSettings: RunnerSettings{
						Executor: "shell",
					},
				},
				JobResponse: JobResponse{
					RunnerInfo: RunnerInfo{
						Timeout: c.jobTimeout,
					},
				},
				SystemInterrupt: make(chan os.Signal),
			}

			trace := Trace{Writer: os.Stdout}
			build.logger = NewBuildLogger(&trace, build.Log())
			sess, err := session.NewSession(nil)
			require.NoError(t, err)
			build.Session = sess

			srv := httptest.NewServer(build.Session.Mux())
			defer srv.Close()

			mockConn := terminal.MockConn{}
			defer mockConn.AssertExpectations(t)
			mockConn.On("Close").Maybe().Return(nil)
			// On Start upgrade the web socket connection and wait for the
			// timeoutCh to exit, to mock real work made on the websocket.
			mockConn.
				On("Start", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) {
					upgrader := &websocket.Upgrader{}
					r := args[1].(*http.Request)
					w := args[0].(http.ResponseWriter)

					_, _ = upgrader.Upgrade(w, r, nil)
					timeoutCh := args[2].(chan error)

					<-timeoutCh
				}).Once()

			mockTerminal := terminal.MockInteractiveTerminal{}
			defer mockTerminal.AssertExpectations(t)
			mockTerminal.On("Connect").Return(&mockConn, nil)
			sess.SetInteractiveTerminal(&mockTerminal)

			u := url.URL{
				Scheme: "ws",
				Host:   srv.Listener.Addr().String(),
				Path:   build.Session.Endpoint + "/exec",
			}
			headers := http.Header{
				"Authorization": []string{build.Session.Token},
			}

			conn, resp, err := websocket.DefaultDialer.Dial(u.String(), headers)
			require.NotNil(t, conn)
			require.NoError(t, err)
			defer func() {
				resp.Body.Close()
				conn.Close()
			}()

			ctx, cancel := context.WithTimeout(context.Background(), build.GetBuildTimeout())

			errCh := make(chan error)
			go func() {
				errCh <- build.waitForTerminal(ctx, c.waitForTerminalTimeout)
			}()

			c.cancelFn(cancel, &build)

			assert.EqualError(t, <-errCh, c.expectedErr)
		})
	}
}

func TestBuild_IsLFSSmudgeDisabled(t *testing.T) {
	testCases := map[string]struct {
		isVariableUnset bool
		variableValue   string
		expectedResult  bool
	}{
		"variable not set": {
			isVariableUnset: true,
			expectedResult:  false,
		},
		"variable empty": {
			variableValue:  "",
			expectedResult: false,
		},
		"variable set to true": {
			variableValue:  "true",
			expectedResult: true,
		},
		"variable set to false": {
			variableValue:  "false",
			expectedResult: false,
		},
		"variable set to 1": {
			variableValue:  "1",
			expectedResult: true,
		},
		"variable set to 0": {
			variableValue:  "0",
			expectedResult: false,
		},
	}

	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			b := &Build{
				JobResponse: JobResponse{
					Variables: JobVariables{},
				},
			}

			if !testCase.isVariableUnset {
				b.Variables = append(
					b.Variables,
					JobVariable{Key: "GIT_LFS_SKIP_SMUDGE", Value: testCase.variableValue, Public: true},
				)
			}

			assert.Equal(t, testCase.expectedResult, b.IsLFSSmudgeDisabled())
		})
	}
}

func TestGitCleanFlags(t *testing.T) {
	tests := map[string]struct {
		value          string
		expectedResult []string
	}{
		"empty clean flags": {
			value:          "",
			expectedResult: []string{"-ffdx"},
		},
		"use custom flags": {
			value:          "custom-flags",
			expectedResult: []string{"custom-flags"},
		},
		"use custom flags with multiple arguments": {
			value:          "-ffdx -e cache/",
			expectedResult: []string{"-ffdx", "-e", "cache/"},
		},
		"disabled": {
			value:          "none",
			expectedResult: []string{},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			build := &Build{
				Runner: &RunnerConfig{},
				JobResponse: JobResponse{
					Variables: JobVariables{
						{Key: "GIT_CLEAN_FLAGS", Value: test.value},
					},
				},
			}

			result := build.GetGitCleanFlags()
			assert.Equal(t, test.expectedResult, result)
		})
	}
}

func TestGitFetchFlags(t *testing.T) {
	tests := map[string]struct {
		value          string
		expectedResult []string
	}{
		"empty fetch flags": {
			value:          "",
			expectedResult: []string{"--prune", "--quiet"},
		},
		"use custom flags": {
			value:          "custom-flags",
			expectedResult: []string{"custom-flags"},
		},
		"use custom flags with multiple arguments": {
			value:          "--prune --tags --quiet",
			expectedResult: []string{"--prune", "--tags", "--quiet"},
		},
		"disabled": {
			value:          "none",
			expectedResult: []string{},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			build := &Build{
				Runner: &RunnerConfig{},
				JobResponse: JobResponse{
					Variables: JobVariables{
						{Key: "GIT_FETCH_EXTRA_FLAGS", Value: test.value},
					},
				},
			}

			result := build.GetGitFetchFlags()
			assert.Equal(t, test.expectedResult, result)
		})
	}
}

func TestDefaultVariables(t *testing.T) {
	tests := map[string]struct {
		jobVariables  JobVariables
		rootDir       string
		key           string
		expectedValue string
	}{
		"get default CI_SERVER value": {
			jobVariables:  JobVariables{},
			rootDir:       "/builds",
			key:           "CI_SERVER",
			expectedValue: "yes",
		},
		"get default CI_PROJECT_DIR value": {
			jobVariables:  JobVariables{},
			rootDir:       "/builds",
			key:           "CI_PROJECT_DIR",
			expectedValue: "/builds/test-namespace/test-repo",
		},
		"get overwritten CI_PROJECT_DIR value": {
			jobVariables: JobVariables{
				{Key: "GIT_CLONE_PATH", Value: "/builds/go/src/gitlab.com/gitlab-org/gitlab-runner", Public: true},
			},
			rootDir:       "/builds",
			key:           "CI_PROJECT_DIR",
			expectedValue: "/builds/go/src/gitlab.com/gitlab-org/gitlab-runner",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			build := Build{
				JobResponse: JobResponse{
					GitInfo: GitInfo{
						RepoURL: "https://gitlab.com/test-namespace/test-repo.git",
					},
					Variables: test.jobVariables,
				},
				Runner: &RunnerConfig{
					RunnerCredentials: RunnerCredentials{
						Token: "1234",
					},
				},
			}

			err := build.StartBuild(test.rootDir, "/cache", true, false)
			assert.NoError(t, err)

			variable := build.GetAllVariables().Get(test.key)
			assert.Equal(t, test.expectedValue, variable)
		})
	}
}

func TestBuildFinishTimeout(t *testing.T) {
	tests := map[string]bool{
		"channel returns first": true,
		"timeout returns first": false,
	}

	for name, chanFirst := range tests {
		t.Run(name, func(t *testing.T) {
			logger, hooks := test.NewNullLogger()
			build := Build{
				logger: NewBuildLogger(nil, logrus.NewEntry(logger)),
			}
			buildFinish := make(chan error, 1)
			timeout := 10 * time.Millisecond

			if chanFirst {
				buildFinish <- errors.New("job finish error")
			}

			build.waitForBuildFinish(buildFinish, timeout)

			entry := hooks.LastEntry()

			if chanFirst {
				assert.Nil(t, entry)
				return
			}

			assert.NotNil(t, entry)
		})
	}
}

func TestProjectUniqueName(t *testing.T) {
	tests := map[string]struct {
		build        Build
		expectedName string
	}{
		"project non rfc1132 unique name": {
			build: Build{
				Runner: &RunnerConfig{
					RunnerCredentials: RunnerCredentials{
						Token: "Ze_n8E6en622WxxSg4r8",
					},
				},
				JobResponse: JobResponse{
					JobInfo: JobInfo{
						ProjectID: 1234567890,
					},
				},
				ProjectRunnerID: 0,
			},
			expectedName: "runner-zen8e6e-project-1234567890-concurrent-0",
		},
		"project non rfc1132 unique name longer than 63 char": {
			build: Build{
				Runner: &RunnerConfig{
					RunnerCredentials: RunnerCredentials{
						Token: "Ze_n8E6en622WxxSg4r8",
					},
				},
				JobResponse: JobResponse{
					JobInfo: JobInfo{
						ProjectID: 123456789012345,
					},
				},
				ProjectRunnerID: 123456789012345,
			},
			expectedName: "runner-zen8e6e-project-123456789012345-concurrent-1234567890123",
		},
		"project normal unique name": {
			build: Build{
				Runner: &RunnerConfig{
					RunnerCredentials: RunnerCredentials{
						Token: "xYzWabc-Ij3xlKjmoPO9",
					},
				},
				JobResponse: JobResponse{
					JobInfo: JobInfo{
						ProjectID: 1234567890,
					},
				},
				ProjectRunnerID: 0,
			},
			expectedName: "runner-xyzwabc--project-1234567890-concurrent-0",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.expectedName, test.build.ProjectUniqueName())
		})
	}
}

func TestBuildStages(t *testing.T) {
	scriptOnlyBuild, err := GetRemoteSuccessfulBuild()
	require.NoError(t, err)

	multistepBuild, err := GetRemoteSuccessfulMultistepBuild()
	require.NoError(t, err)

	tests := map[string]struct {
		jobResponse    JobResponse
		expectedStages []BuildStage
	}{
		"script only build": {
			jobResponse:    scriptOnlyBuild,
			expectedStages: append(staticBuildStages, "step_script"),
		},
		"multistep build": {
			jobResponse:    multistepBuild,
			expectedStages: append(staticBuildStages, "step_script", "step_release"),
		},
	}

	for tn, tt := range tests {
		t.Run(tn, func(t *testing.T) {
			build := &Build{
				JobResponse: tt.jobResponse,
			}
			assert.ElementsMatch(t, tt.expectedStages, build.BuildStages())
		})
	}
}

func TestBuild_GetExecutorJobSectionAttempts(t *testing.T) {
	tests := []struct {
		attempts         string
		expectedAttempts int
		expectedErr      error
	}{
		{
			attempts:         "",
			expectedAttempts: 1,
		},
		{
			attempts:         "3",
			expectedAttempts: 3,
		},
		{
			attempts:         "0",
			expectedAttempts: 0,
			expectedErr:      &invalidAttemptError{},
		},
		{
			attempts:         "99",
			expectedAttempts: 0,
			expectedErr:      &invalidAttemptError{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.attempts, func(t *testing.T) {
			build := Build{
				JobResponse: JobResponse{
					Variables: JobVariables{
						JobVariable{
							Key:   ExecutorJobSectionAttempts,
							Value: tt.attempts,
						},
					},
				},
			}

			attempts, err := build.GetExecutorJobSectionAttempts()
			assert.True(t, errors.Is(err, tt.expectedErr))
			assert.Equal(t, tt.expectedAttempts, attempts)
		})
	}
}

func setupSuccessfulMockExecutor(
	t *testing.T,
	prepareFn func(options ExecutorPrepareOptions) error,
) (*MockExecutorProvider, func()) {
	executor, provider := setupMockExecutorAndProvider()
	assertFn := func() {
		executor.AssertExpectations(t)
		provider.AssertExpectations(t)
	}

	// We run everything once
	executor.On("Prepare", mock.Anything).Return(prepareFn).Once()
	executor.On("Finish", nil).Once()
	executor.On("Cleanup").Once()

	// Run script successfully
	executor.On("Shell").Return(&ShellScriptInfo{Shell: "script-shell"})
	executor.On("Run", matchBuildStage(BuildStagePrepare)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageGetSources)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageRestoreCache)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageDownloadArtifacts)).Return(nil).Once()
	executor.On("Run", matchBuildStage("step_script")).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageAfterScript)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageArchiveCache)).Return(nil).Once()
	executor.On("Run", matchBuildStage(BuildStageUploadOnSuccessArtifacts)).
		Return(nil).
		Once()

	return provider, assertFn
}

func setupMockExecutorAndProvider() (*MockExecutor, *MockExecutorProvider) {
	e := new(MockExecutor)
	p := new(MockExecutorProvider)

	p.On("CanCreate").Return(true).Once()
	p.On("GetDefaultShell").Return("bash").Once()
	p.On("GetFeatures", mock.Anything).Return(nil).Twice()
	p.On("Create").Return(e).Once()

	return e, p
}

func registerExecutorWithSuccessfulBuild(t *testing.T, p *MockExecutorProvider, rc *RunnerConfig) *Build {
	require.NotNil(t, rc)

	RegisterExecutorProvider(t.Name(), p)

	successfulBuild, err := GetSuccessfulBuild()
	require.NoError(t, err)
	if rc.RunnerSettings.Executor == "" {
		// Ensure we set the executor name if not already defined
		rc.RunnerSettings.Executor = t.Name()
	}
	build, err := NewBuild(successfulBuild, rc, nil, nil)
	assert.NoError(t, err)
	return build
}

func runSuccessfulMockBuild(t *testing.T, prepareFn func(options ExecutorPrepareOptions) error) *Build {
	p, assertFn := setupSuccessfulMockExecutor(t, prepareFn)
	defer assertFn()

	build := registerExecutorWithSuccessfulBuild(t, p, new(RunnerConfig))
	err := build.Run(&Config{}, &Trace{Writer: os.Stdout})
	assert.NoError(t, err)

	return build
}
