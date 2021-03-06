package cell_test

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"time"

	"github.com/pivotal-golang/archiver/extractor/test_helper"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/inigo/helpers"
	"github.com/cloudfoundry-incubator/inigo/inigo_announcement_server"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/rep"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry-incubator/runtime-schema/models/factories"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
)

var _ = Describe("Executor", func() {
	var (
		executorProcess, fileServerProcess, repProcess, auctioneerProcess, convergerProcess ifrit.Process
	)

	var fileServerStaticDir string

	BeforeEach(func() {
		var fileServerRunner ifrit.Runner

		fileServerRunner, fileServerStaticDir = componentMaker.FileServer()

		executorProcess = ginkgomon.Invoke(componentMaker.Executor("-memoryMB", "1024"))
		fileServerProcess = ginkgomon.Invoke(fileServerRunner)
		repProcess = ginkgomon.Invoke(componentMaker.Rep())
		auctioneerProcess = ginkgomon.Invoke(componentMaker.Auctioneer())
		convergerProcess = ginkgomon.Invoke(componentMaker.Converger())
	})

	AfterEach(func() {
		helpers.StopProcesses(executorProcess, fileServerProcess, repProcess, auctioneerProcess, convergerProcess)
	})

	Describe("Heartbeating", func() {
		It("should heartbeat its presence (through the rep)", func() {
			Eventually(receptorClient.Cells).Should(HaveLen(1))
		})
	})

	Describe("Resource limits", func() {
		It("should only pick up tasks if it has capacity", func() {
			firstGuyGuid := factories.GenerateGuid()
			secondGuyGuid := factories.GenerateGuid()

			err := receptorClient.CreateTask(receptor.TaskCreateRequest{
				TaskGuid: firstGuyGuid,
				Domain:   INIGO_DOMAIN,
				Stack:    componentMaker.Stack,
				MemoryMB: 1024,
				DiskMB:   1024,
				Action: &models.RunAction{
					Path: "/bin/bash",
					Args: []string{"-c", "curl " + inigo_announcement_server.AnnounceURL(firstGuyGuid) + " && tail -f /dev/null"},
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(inigo_announcement_server.Announcements).Should(ContainElement(firstGuyGuid))

			err = receptorClient.CreateTask(receptor.TaskCreateRequest{
				TaskGuid: secondGuyGuid,
				Domain:   INIGO_DOMAIN,
				Stack:    componentMaker.Stack,
				MemoryMB: 1024,
				DiskMB:   1024,
				Action: &models.RunAction{
					Path: "curl",
					Args: []string{inigo_announcement_server.AnnounceURL(secondGuyGuid)},
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			Consistently(inigo_announcement_server.Announcements).ShouldNot(ContainElement(secondGuyGuid))
		})
	})

	Describe("consistency", func() {
		Context("when a task is running and then something causes the container to go away (e.g. executor restart)", func() {
			var taskGuid string

			BeforeEach(func() {
				taskGuid = factories.GenerateGuid()

				err := receptorClient.CreateTask(receptor.TaskCreateRequest{
					TaskGuid: taskGuid,
					Domain:   INIGO_DOMAIN,
					Stack:    componentMaker.Stack,
					Action: &models.RunAction{
						Path: "sh",
						Args: []string{"-c", "while true; do sleep 1; done"},
					},
				})
				Ω(err).ShouldNot(HaveOccurred())

				executorClient := componentMaker.ExecutorClient()

				Eventually(func() executor.State {
					container, err := executorClient.GetContainer(taskGuid)
					if err == nil {
						return container.State
					}
					return executor.StateInvalid
				}).Should(Equal(executor.StateRunning))

				// bounce executor
				ginkgomon.Kill(executorProcess)
				executorProcess = ginkgomon.Invoke(componentMaker.Executor("-memoryMB", "1024"))
			})

			It("eventually marks the task completed and failed", func() {
				var task receptor.TaskResponse

				Eventually(func() interface{} {
					var err error

					task, err = receptorClient.GetTask(taskGuid)
					Ω(err).ShouldNot(HaveOccurred())

					return task.State
				}).Should(Equal(receptor.TaskStateCompleted))

				Ω(task.Failed).Should(BeTrue())
			})
		})

		Context("when a lrp is running and then something causes the container to go away", func() {
			var (
				instanceGuid string
				processGuid  string
				index        int
			)

			BeforeEach(func() {
				processGuid = factories.GenerateGuid()
				index = 0

				err := receptorClient.CreateDesiredLRP(receptor.DesiredLRPCreateRequest{
					Domain:      INIGO_DOMAIN,
					ProcessGuid: processGuid,
					Instances:   1,
					Stack:       componentMaker.Stack,
					MemoryMB:    128,
					DiskMB:      1024,
					Ports:       []uint16{8080},
					Action: &models.RunAction{
						Path: "sh",
						Args: []string{
							"-c",
							"while true; do sleep 1; done",
						},
					},
					Monitor: &models.RunAction{
						Path: "sh",
						Args: []string{"-c", "echo all good"},
					},
				})
				Ω(err).ShouldNot(HaveOccurred())

				var actualLRPs []receptor.ActualLRPResponse
				Eventually(func() interface{} {
					actualLRPs = helpers.ActiveActualLRPs(receptorClient, processGuid)
					return actualLRPs
				}).Should(HaveLen(1))

				instanceGuid = actualLRPs[0].InstanceGuid
				containerGuid := rep.LRPContainerGuid(processGuid, instanceGuid)

				executorClient := componentMaker.ExecutorClient()

				Eventually(func() executor.State {
					container, err := executorClient.GetContainer(containerGuid)
					if err == nil {
						return container.State
					}

					return executor.StateInvalid
				}).Should(Equal(executor.StateRunning))

				// bounce executor
				ginkgomon.Kill(executorProcess)
				executorProcess = ginkgomon.Invoke(componentMaker.Executor("-memoryMB", "1024"))
			})

			It("eventually deletes the original lrp", func() {
				lrpMatchesOriginalInstanceGuid := func() (bool, error) {
					actualLRP, err := receptorClient.ActualLRPByProcessGuidAndIndex(processGuid, index)
					if err != nil {
						return false, err
					}
					return actualLRP.InstanceGuid == instanceGuid, nil
				}

				Eventually(lrpMatchesOriginalInstanceGuid).Should(BeFalse())
			})
		})
	})

	Describe("Stack", func() {
		var wrongStack = "penguin"

		It("should only pick up tasks if the stacks match", func() {
			matchingGuid := factories.GenerateGuid()
			nonMatchingGuid := factories.GenerateGuid()

			err := receptorClient.CreateTask(receptor.TaskCreateRequest{
				TaskGuid: matchingGuid,
				Domain:   INIGO_DOMAIN,
				Stack:    componentMaker.Stack,
				Action: &models.RunAction{
					Path: "curl",
					Args: []string{inigo_announcement_server.AnnounceURL(matchingGuid)},
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			err = receptorClient.CreateTask(receptor.TaskCreateRequest{
				TaskGuid: nonMatchingGuid,
				Domain:   INIGO_DOMAIN,
				Stack:    wrongStack,
				Action: &models.RunAction{
					Path: "curl",
					Args: []string{inigo_announcement_server.AnnounceURL(nonMatchingGuid)},
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			Consistently(inigo_announcement_server.Announcements).ShouldNot(ContainElement(nonMatchingGuid), "Did not expect to see this app running, as it has the wrong stack.")
			Eventually(inigo_announcement_server.Announcements).Should(ContainElement(matchingGuid))
		})
	})

	Describe("Running a task", func() {
		var guid string

		BeforeEach(func() {
			guid = factories.GenerateGuid()
		})

		It("runs the command with the provided environment", func() {
			err := receptorClient.CreateTask(receptor.TaskCreateRequest{
				TaskGuid: guid,
				Domain:   INIGO_DOMAIN,
				Stack:    componentMaker.Stack,
				Action: &models.RunAction{
					Path: "sh",
					Args: []string{"-c", `[ "$FOO" = NEW-BAR -a "$BAZ" = WIBBLE ]`},
					Env: []models.EnvironmentVariable{
						{"FOO", "OLD-BAR"},
						{"BAZ", "WIBBLE"},
						{"FOO", "NEW-BAR"},
					},
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			var task receptor.TaskResponse

			Eventually(func() interface{} {
				var err error

				task, err = receptorClient.GetTask(guid)
				Ω(err).ShouldNot(HaveOccurred())

				return task.State
			}).Should(Equal(receptor.TaskStateCompleted))

			Ω(task.Failed).Should(BeFalse())
		})

		It("runs the command with the provided working directory", func() {
			err := receptorClient.CreateTask(receptor.TaskCreateRequest{
				TaskGuid: guid,
				Domain:   INIGO_DOMAIN,
				Stack:    componentMaker.Stack,
				Action: &models.RunAction{
					Path: "sh",
					Args: []string{"-c", `[ $PWD = /tmp ]`},
					Dir:  "/tmp",
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			var task receptor.TaskResponse

			Eventually(func() interface{} {
				var err error

				task, err = receptorClient.GetTask(guid)
				Ω(err).ShouldNot(HaveOccurred())

				return task.State
			}).Should(Equal(receptor.TaskStateCompleted))

			Ω(task.Failed).Should(BeFalse())
		})

		Context("when the command exceeds its memory limit", func() {
			It("should fail the Task", func() {
				err := receptorClient.CreateTask(receptor.TaskCreateRequest{
					Domain:   INIGO_DOMAIN,
					TaskGuid: guid,
					Stack:    componentMaker.Stack,
					MemoryMB: 10,
					DiskMB:   1024,
					Action: models.Serial(
						&models.RunAction{
							Path: "curl",
							Args: []string{inigo_announcement_server.AnnounceURL("before-memory-overdose")},
						},
						&models.RunAction{
							Path: "sh",
							Args: []string{"-c", "yes $(yes)"},
						},
						&models.RunAction{
							Path: "curl",
							Args: []string{inigo_announcement_server.AnnounceURL("after-memory-overdose")},
						},
					),
				})
				Ω(err).ShouldNot(HaveOccurred())

				Eventually(inigo_announcement_server.Announcements).Should(ContainElement("before-memory-overdose"))

				var task receptor.TaskResponse
				Eventually(func() interface{} {
					var err error

					task, err = receptorClient.GetTask(guid)
					Ω(err).ShouldNot(HaveOccurred())

					return task.State
				}).Should(Equal(receptor.TaskStateCompleted))

				Ω(task.Failed).Should(BeTrue())
				Ω(task.FailureReason).Should(ContainSubstring("out of memory"))

				Ω(inigo_announcement_server.Announcements()).ShouldNot(ContainElement("after-memory-overdose"))
			})
		})

		Context("when the command exceeds its file descriptor limit", func() {
			It("should fail the Task", func() {
				nofile := uint64(10)

				err := receptorClient.CreateTask(receptor.TaskCreateRequest{
					Domain:   INIGO_DOMAIN,
					TaskGuid: guid,
					Stack:    componentMaker.Stack,
					Action: models.Serial(
						&models.RunAction{
							Path: "sh",
							Args: []string{"-c", `
set -e

# must start after fd 2
exec 3<>file1
exec 4<>file2
exec 5<>file3
exec 6<>file4
exec 7<>file5
exec 8<>file6
exec 9<>file7
exec 10<>file8
exec 11<>file9
exec 12<>file10
exec 13<>file11

echo should have died by now
`},
							ResourceLimits: models.ResourceLimits{
								Nofile: &nofile,
							},
						},
					),
				})
				Ω(err).ShouldNot(HaveOccurred())

				var task receptor.TaskResponse
				Eventually(func() interface{} {
					var err error

					task, err = receptorClient.GetTask(guid)
					Ω(err).ShouldNot(HaveOccurred())

					return task.State
				}).Should(Equal(receptor.TaskStateCompleted))

				Ω(task.Failed).Should(BeTrue())

				// when sh can't open another file the exec exits 2
				Ω(task.FailureReason).Should(ContainSubstring("status 2"))
			})
		})

		Context("when the command times out", func() {
			It("should fail the Task", func() {
				err := receptorClient.CreateTask(receptor.TaskCreateRequest{
					Domain:   INIGO_DOMAIN,
					TaskGuid: guid,
					Stack:    componentMaker.Stack,
					Action: models.Serial(
						models.Timeout(
							&models.RunAction{
								Path: "sleep",
								Args: []string{"1"},
							},
							500*time.Millisecond,
						),
					),
				})
				Ω(err).ShouldNot(HaveOccurred())

				var task receptor.TaskResponse
				Eventually(func() interface{} {
					var err error

					task, err = receptorClient.GetTask(guid)
					Ω(err).ShouldNot(HaveOccurred())

					return task.State
				}).Should(Equal(receptor.TaskStateCompleted))

				Ω(task.Failed).Should(BeTrue())
				Ω(task.FailureReason).Should(ContainSubstring("exceeded 500ms timeout"))
			})
		})
	})

	Describe("Running a downloaded file", func() {
		var guid string

		BeforeEach(func() {
			guid = factories.GenerateGuid()

			test_helper.CreateTarGZArchive(filepath.Join(fileServerStaticDir, "announce.tar.gz"), []test_helper.ArchiveFile{
				{
					Name: "announce",
					Body: fmt.Sprintf("#!/bin/sh\n\ncurl %s", inigo_announcement_server.AnnounceURL(guid)),
					Mode: 0755,
				},
			})
		})

		It("downloads the file", func() {
			err := receptorClient.CreateTask(receptor.TaskCreateRequest{
				Domain:   INIGO_DOMAIN,
				TaskGuid: guid,
				Stack:    componentMaker.Stack,
				Action: models.Serial(
					&models.DownloadAction{
						From: fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "announce.tar.gz"),
						To:   ".",
					},
					&models.RunAction{
						Path: "./announce",
					},
				),
			})
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(inigo_announcement_server.Announcements).Should(ContainElement(guid))
		})
	})

	Describe("Uploading from the container", func() {
		var guid string

		var server *httptest.Server
		var uploadAddr string

		var gotRequest chan struct{}

		BeforeEach(func() {
			guid = factories.GenerateGuid()

			gotRequest = make(chan struct{})

			server, uploadAddr = helpers.Callback(componentMaker.ExternalAddress, ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", "/thingy"),
				func(w http.ResponseWriter, r *http.Request) {
					contents, err := ioutil.ReadAll(r.Body)
					Ω(err).ShouldNot(HaveOccurred())

					Ω(string(contents)).Should(Equal("tasty thingy\n"))

					close(gotRequest)
				},
			))
		})

		AfterEach(func() {
			server.Close()
		})

		It("uploads the specified files", func() {
			err := receptorClient.CreateTask(receptor.TaskCreateRequest{
				Domain:   INIGO_DOMAIN,
				TaskGuid: guid,
				Stack:    componentMaker.Stack,
				Action: models.Serial(
					&models.RunAction{
						Path: "sh",
						Args: []string{"-c", "echo tasty thingy > thingy"},
					},
					&models.UploadAction{
						From: "thingy",
						To:   fmt.Sprintf("http://%s/thingy", uploadAddr),
					},
					&models.RunAction{
						Path: "curl",
						Args: []string{inigo_announcement_server.AnnounceURL(guid)},
					},
				),
			})
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(gotRequest).Should(BeClosed())

			Eventually(inigo_announcement_server.Announcements).Should(ContainElement(guid))
		})
	})

	Describe("Fetching results", func() {
		It("should fetch the contents of the requested file and provide the content in the completed Task", func() {
			guid := factories.GenerateGuid()

			err := receptorClient.CreateTask(receptor.TaskCreateRequest{
				Domain:     INIGO_DOMAIN,
				TaskGuid:   guid,
				Stack:      componentMaker.Stack,
				ResultFile: "thingy",
				Action: &models.RunAction{
					Path: "sh",
					Args: []string{"-c", "echo tasty thingy > thingy"},
				},
			})
			Ω(err).ShouldNot(HaveOccurred())

			var task receptor.TaskResponse
			Eventually(func() interface{} {
				var err error

				task, err = receptorClient.GetTask(guid)
				Ω(err).ShouldNot(HaveOccurred())

				return task.State
			}).Should(Equal(receptor.TaskStateCompleted))

			Ω(task.Result).Should(Equal("tasty thingy\n"))
		})
	})
})
