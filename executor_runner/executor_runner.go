package executor_runner

import (
	"fmt"
	"github.com/cloudfoundry-incubator/inigo/runner_support"
	"github.com/onsi/ginkgo/config"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/gomega"
	"github.com/vito/cmdtest"
	. "github.com/vito/cmdtest/matchers"
)

type ExecutorRunner struct {
	executorBin   string
	wardenNetwork string
	wardenAddr    string
	etcdMachines  []string
	snapshotFile  string

	Session *cmdtest.Session
}

func New(executorBin, wardenNetwork, wardenAddr string, etcdMachines []string) *ExecutorRunner {
	return &ExecutorRunner{
		executorBin:   executorBin,
		wardenNetwork: wardenNetwork,
		wardenAddr:    wardenAddr,
		etcdMachines:  etcdMachines,
	}
}

func (r *ExecutorRunner) Start(memoryMB int, diskMB int) {
	r.StartWithoutCheck(memoryMB, diskMB, fmt.Sprintf("/tmp/executor_registry_%d", config.GinkgoConfig.ParallelNode))

	Ω(r.Session).Should(SayWithTimeout("Watching for RunOnces!", 1*time.Second))
}

func (r *ExecutorRunner) StartWithoutCheck(memoryMB int, diskMB int, snapshotFile string) {
	executorSession, err := cmdtest.StartWrapped(
		exec.Command(
			r.executorBin,
			"-wardenNetwork", r.wardenNetwork,
			"-wardenAddr", r.wardenAddr,
			"-etcdMachines", strings.Join(r.etcdMachines, ","),
			"-memoryMB", fmt.Sprintf("%d", memoryMB),
			"-diskMB", fmt.Sprintf("%d", diskMB),
			"-registrySnapshotFile", snapshotFile,
		),
		runner_support.TeeIfVerbose,
		runner_support.TeeIfVerbose,
	)
	Ω(err).ShouldNot(HaveOccurred())
	r.snapshotFile = snapshotFile
	r.Session = executorSession
}

func (r *ExecutorRunner) Stop() {
	if r.Session != nil {
		r.Session.Cmd.Process.Signal(syscall.SIGTERM)
		os.Remove(r.snapshotFile)
	}
}
