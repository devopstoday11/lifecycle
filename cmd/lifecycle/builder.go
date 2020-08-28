package main

import (
	"errors"
	"fmt"
	"github.com/BurntSushi/toml"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/buildpacks/lifecycle"
	"github.com/buildpacks/lifecycle/api"
	"github.com/buildpacks/lifecycle/cmd"
	"github.com/buildpacks/lifecycle/env"
	"github.com/buildpacks/lifecycle/launch"
	"github.com/buildpacks/lifecycle/priv"
	"github.com/buildpacks/lifecycle/snapshot"
)

type buildCmd struct {
	// flags: inputs
	stackGroupPath string
	buildArgs
}

type buildArgs struct {
	// inputs needed when run by creator
	uid, gid      int
	groupPath     string
	planPath      string
	buildpacksDir string
	layersDir     string
	appDir        string
	platformDir   string
	platformAPI   string
}

func (b *buildCmd) Init() {
	cmd.FlagBuildpacksDir(&b.buildpacksDir)
	cmd.FlagGroupPath(&b.groupPath)
	cmd.FlagPlanPath(&b.planPath)
	cmd.FlagLayersDir(&b.layersDir)
	cmd.FlagAppDir(&b.appDir)
	cmd.FlagPlatformDir(&b.platformDir)
	cmd.FlagStackGroupPath(&b.stackGroupPath)
}

func (b *buildCmd) Args(nargs int, args []string) error {
	if nargs != 0 {
		return cmd.FailErrCode(errors.New("received unexpected arguments"), cmd.CodeInvalidArgs, "parse arguments")
	}
	return nil
}

func (b *buildCmd) Privileges() error {
	_, stackGroup, _, err := b.readData()
	if err != nil {
		return err
	}

	// builder should never be run with privileges if there aren't any stack buildpacks
	if len(stackGroup.Group) == 0 && priv.IsPrivileged() {
		return cmd.FailErr(errors.New("refusing to run as root"), "build")
	}
	return nil
}

func (b *buildCmd) Exec() error {
	group, stackGroup, plan, err := b.readData()
	if err != nil {
		return err
	}

	if len(stackGroup.Group) > 0 {
		return b.buildAll(group, stackGroup, plan)
	}
	return b.buildAsSubProcess()
}

func (ba buildArgs) buildAll(group, stackGroup lifecycle.BuildpackGroup, plan lifecycle.BuildPlan) error {
	if err := verifyBuildpackApis(group); err != nil {
		return err
	}

	if err := verifyBuildpackApis(stackGroup); err != nil {
		return err
	}

	builder, err := ba.createBuilder(group, stackGroup, plan)
	if err != nil {
		return err
	}

	if err = ba.stackBuild(builder); err != nil {
		return err
	}

	if len(group.Group) > 0 {
		if err = ba.buildAsSubProcess(); err != nil {
			return err
		}
	}
	return nil
}

func (ba buildArgs) stackBuild(builder *lifecycle.Builder) error {
	// run stack buildpacks as root
	_, err := builder.StackBuild()
	if err != nil {
		if err, ok := err.(*lifecycle.Error); ok {
			if err.Type == lifecycle.ErrTypeBuildpack {
				return cmd.FailErrCode(err.Cause(), cmd.CodeFailedBuildWithErrors, "stack-build")
			}
		}
		return cmd.FailErrCode(err, cmd.CodeBuildError, "stack-build")
	}
	return nil
}

func (ba buildArgs) buildAsSubProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	c := exec.Command(
		filepath.Join(filepath.Dir(exe), "builder"),
		fmt.Sprintf("-%s", cmd.FlagNameGroupPath), ba.groupPath,
		fmt.Sprintf("-%s", cmd.FlagNamePlanPath), ba.planPath,
		// TODO set other args
	)
	c.SysProcAttr = &syscall.SysProcAttr{}
	c.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(ba.uid), Gid: uint32(ba.gid)}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	return c.Run()
}

func (ba buildArgs) build(builder *lifecycle.Builder) error {
	// drop back to non-root user to run buildpacks
	if err := priv.RunAs(ba.uid, ba.gid); err != nil {
		return cmd.FailErr(err, fmt.Sprintf("exec as user %d:%d", ba.uid, ba.gid))
	}
	if err := priv.SetEnvironmentForUser(ba.uid); err != nil {
		return cmd.FailErr(err, fmt.Sprintf("set environment for user %d", ba.uid))
	}
	md, err := builder.Build()
	if err != nil {
		if err, ok := err.(*lifecycle.Error); ok {
			if err.Type == lifecycle.ErrTypeBuildpack {
				return cmd.FailErrCode(err.Cause(), cmd.CodeFailedBuildWithErrors, "build")
			}
		}
		return cmd.FailErrCode(err, cmd.CodeBuildError, "build")
	}
	if err := lifecycle.WriteTOML(launch.GetMetadataFilePath(ba.layersDir), md); err != nil {
		return cmd.FailErr(err, "write build metadata")
	}
	return nil
}

func (ba buildArgs) createBuilder(group, stackGroup lifecycle.BuildpackGroup, plan lifecycle.BuildPlan) (*lifecycle.Builder, error) {
	stackSnapshotter, err := snapshot.NewKanikoSnapshotter("/")
	if err != nil {
		return &lifecycle.Builder{}, err
	}

	return &lifecycle.Builder{
		AppDir:        ba.appDir,
		LayersDir:     ba.layersDir,
		PlatformDir:   ba.platformDir,
		BuildpacksDir: ba.buildpacksDir,
		PlatformAPI:   api.MustParse(ba.platformAPI),
		Env:           env.NewBuildEnv(os.Environ()),
		Group:         group,
		StackGroup:    stackGroup,
		Plan:          plan,
		Out:           log.New(os.Stdout, "", 0),
		Err:           log.New(os.Stderr, "", 0),
		Snapshotter:   stackSnapshotter,
	}, nil
}

func (b *buildCmd) readData() (lifecycle.BuildpackGroup, lifecycle.BuildpackGroup, lifecycle.BuildPlan, error) {
	group := lifecycle.BuildpackGroup{}
	if _, err := os.Stat(b.groupPath); err == nil {
		group, err = lifecycle.ReadGroup(b.groupPath)
		if err != nil {
			return lifecycle.BuildpackGroup{}, lifecycle.BuildpackGroup{}, lifecycle.BuildPlan{}, cmd.FailErr(err, "read buildpack group")
		}
	}

	stackGroup := lifecycle.BuildpackGroup{}
	if _, err := os.Stat(b.stackGroupPath); err == nil {
		stackGroup, err = lifecycle.ReadGroup(b.stackGroupPath)
		if err != nil {
			return group, lifecycle.BuildpackGroup{}, lifecycle.BuildPlan{}, cmd.FailErr(err, "read stack buildpack group")
		}
	}

	var plan lifecycle.BuildPlan
	if _, err := toml.DecodeFile(b.planPath, &plan); err != nil {
		return lifecycle.BuildpackGroup{}, lifecycle.BuildpackGroup{}, lifecycle.BuildPlan{}, cmd.FailErr(err, "parse detect plan")
	}
	return group, stackGroup, plan, nil
}
