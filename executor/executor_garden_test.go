package executor_test

import (
	"archive/tar"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/cf-test-helpers/generator"
	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/executor/gardenhealth"
	executorinit "github.com/cloudfoundry-incubator/executor/initializer"
	uuid "github.com/nu7hatch/gouuid"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	"github.com/cloudfoundry-incubator/garden"
)

var _ = Describe("Executor/Garden", func() {
	const pruningInterval = 500 * time.Millisecond
	var (
		executorClient       executor.Client
		process              ifrit.Process
		runner               ifrit.Runner
		gardenCapacity       garden.Capacity
		exportNetworkEnvVars bool
		cachePath            string
		config               executorinit.Configuration
		logger               lager.Logger
		ownerName            string
	)

	BeforeEach(func() {
		config = executorinit.DefaultConfiguration

		var err error
		cachePath, err = ioutil.TempDir("", "executor-tmp")
		Expect(err).NotTo(HaveOccurred())

		ownerName = "executor" + generator.RandomName()
	})

	JustBeforeEach(func() {
		var err error

		config.GardenNetwork = "tcp"
		config.GardenAddr = componentMaker.Addresses.GardenLinux
		config.ReservedExpirationTime = pruningInterval
		config.ContainerReapInterval = pruningInterval
		config.HealthyMonitoringInterval = time.Second
		config.UnhealthyMonitoringInterval = 100 * time.Millisecond
		config.ExportNetworkEnvVars = exportNetworkEnvVars
		config.ContainerOwnerName = ownerName
		config.CachePath = cachePath
		config.GardenHealthcheckProcessPath = "/bin/sh"
		config.GardenHealthcheckProcessArgs = []string{"-c", "echo", "checking health"}
		config.GardenHealthcheckProcessUser = "vcap"
		logger = lagertest.NewTestLogger("test")
		var executorMembers grouper.Members
		executorClient, executorMembers, err = executorinit.Initialize(logger, config, clock.NewClock())
		Expect(err).NotTo(HaveOccurred())
		runner = grouper.NewParallel(os.Kill, executorMembers)

		gardenCapacity, err = gardenClient.Capacity()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if executorClient != nil {
			executorClient.Cleanup(logger)
		}

		if process != nil {
			ginkgomon.Kill(process)
		}

		os.RemoveAll(cachePath)
	})

	generateGuid := func() string {
		id, err := uuid.NewV4()
		Expect(err).NotTo(HaveOccurred())

		return id.String()
	}

	allocNewContainer := func(container executor.Container) string {
		container.Guid = generateGuid()

		request := executor.NewAllocationRequest(container.Guid, &container.Resource, container.Tags)
		failures, err := executorClient.AllocateContainers(logger, []executor.AllocationRequest{request})
		Expect(failures).To(BeEmpty())
		Expect(err).NotTo(HaveOccurred())

		return request.Guid
	}

	getContainer := func(guid string) executor.Container {
		container, err := executorClient.GetContainer(logger, guid)
		Expect(err).NotTo(HaveOccurred())

		return container
	}

	containerStatePoller := func(guid string) func() executor.State {
		return func() executor.State {
			return getContainer(guid).State
		}
	}

	containerEventPoller := func(eventSource executor.EventSource, event *executor.Event) func() executor.EventType {
		return func() executor.EventType {
			var err error
			*event, err = eventSource.Next()
			Expect(err).NotTo(HaveOccurred())
			return (*event).EventType()
		}
	}

	findGardenContainer := func(handle string) garden.Container {
		var container garden.Container

		Eventually(func() error {
			var err error

			container, err = gardenClient.Lookup(handle)
			return err
		}).ShouldNot(HaveOccurred())

		return container
	}

	removeHealthcheckContainers := func(containers []executor.Container) []executor.Container {
		newContainers := []executor.Container{}
		for i := range containers {
			if !containers[i].HasTags(executor.Tags{gardenhealth.HealthcheckTag: gardenhealth.HealthcheckTagValue}) {
				newContainers = append(newContainers, containers[i])
			}
		}

		return newContainers
	}

	Describe("Starting up", func() {
		BeforeEach(func() {
			os.RemoveAll(cachePath)
		})

		JustBeforeEach(func() {
			process = ginkgomon.Invoke(runner)
		})

		Context("when the cache directory exists and contains files", func() {
			BeforeEach(func() {
				err := os.MkdirAll(cachePath, 0755)
				Expect(err).NotTo(HaveOccurred())

				err = ioutil.WriteFile(filepath.Join(cachePath, "should-get-deleted"), []byte("some-contents"), 0755)
				Expect(err).NotTo(HaveOccurred())
			})

			It("clears it out", func() {
				Eventually(func() []string {
					files, err := ioutil.ReadDir(cachePath)
					if err != nil {
						return nil
					}

					filenames := make([]string, len(files))
					for i := 0; i < len(files); i++ {
						filenames[i] = files[i].Name()
					}

					return filenames
				}, 10*time.Second).Should(BeEmpty())
			})
		})

		Context("when the cache directory doesn't exist", func() {
			It("creates a new cache directory", func() {
				Eventually(func() bool {
					dirInfo, err := os.Stat(cachePath)
					if err != nil {
						return false
					}

					return dirInfo.IsDir()
				}, 10*time.Second).Should(BeTrue())
			})
		})

		Context("when there are containers that are owned by the executor", func() {
			var container1, container2 garden.Container

			BeforeEach(func() {
				var err error

				container1, err = gardenClient.Create(garden.ContainerSpec{
					Properties: garden.Properties{
						"executor:owner": ownerName,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				container2, err = gardenClient.Create(garden.ContainerSpec{
					Properties: garden.Properties{
						"executor:owner": ownerName,
					},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("deletes those containers (and only those containers)", func() {
				Eventually(func() error {
					_, err := gardenClient.Lookup(container1.Handle())
					return err
				}).Should(HaveOccurred())

				Eventually(func() error {
					_, err := gardenClient.Lookup(container2.Handle())
					return err
				}).Should(HaveOccurred())
			})
		})
	})

	Describe("Failing start up", func() {
		BeforeEach(func() {
			config.GardenHealthcheckRootFS = "/bad/path"
			config.GardenHealthcheckInterval = 5 * time.Millisecond
			config.GardenHealthcheckCommandRetryPause = 5 * time.Millisecond
		})

		It("shuts down the executor", func() {
			process = ifrit.Background(runner)
			processExit := process.Wait()

			Eventually(processExit).Should(Receive(HaveOccurred()))
		})
	})

	Describe("Running", func() {
		JustBeforeEach(func() {
			process = ginkgomon.Invoke(runner)
		})

		Describe("Checking garden's health", func() {
			Context("container creation succeeds", func() {
				It("reports healthy", func() {
					Expect(executorClient.Healthy(logger)).Should(BeTrue())
				})
			})

			Context("garden fails then resolves the problem", func() {
				BeforeEach(func() {
					config.GardenHealthcheckInterval = 5 * time.Millisecond
				})

				It("reports correctly", func() {
					Expect(executorClient.Healthy(logger)).Should(BeTrue())

					isHealthy := func() bool { return executorClient.Healthy(logger) }

					ginkgomon.Interrupt(gardenProcess)
					Eventually(isHealthy).Should(BeFalse())

					gardenProcess = ginkgomon.Invoke(componentMaker.GardenLinux())
					Eventually(isHealthy).Should(BeTrue())
				})
			})
		})

		Describe("pinging the server", func() {
			var pingErr error

			Context("when Garden responds to ping", func() {
				JustBeforeEach(func() {
					pingErr = executorClient.Ping(logger)
				})

				It("does not return an error", func() {
					Expect(pingErr).NotTo(HaveOccurred())
				})
			})

			Context("when Garden returns an error", func() {
				JustBeforeEach(func() {
					ginkgomon.Interrupt(gardenProcess)
					pingErr = executorClient.Ping(logger)
				})

				AfterEach(func() {
					gardenProcess = ginkgomon.Invoke(componentMaker.GardenLinux())
				})

				It("should return an error", func() {
					Expect(pingErr).To(HaveOccurred())
					Expect(pingErr.Error()).To(ContainSubstring("connection refused"))
				})
			})
		})

		Describe("getting the total resources", func() {
			var resources executor.ExecutorResources
			var resourceErr error

			JustBeforeEach(func() {
				resources, resourceErr = executorClient.TotalResources(logger)
			})

			It("not return an error", func() {
				Expect(resourceErr).NotTo(HaveOccurred())
			})

			It("returns the preset capacity", func() {
				expectedResources := executor.ExecutorResources{
					MemoryMB:   int(gardenCapacity.MemoryInBytes / 1024 / 1024),
					DiskMB:     int(gardenCapacity.DiskInBytes / 1024 / 1024),
					Containers: int(gardenCapacity.MaxContainers),
				}
				Expect(resources).To(Equal(expectedResources))
			})
		})

		Describe("allocating a container", func() {
			var (
				allocationRequest executor.AllocationRequest

				guid string

				allocationFailures []executor.AllocationFailure
				allocErr           error
			)

			BeforeEach(func() {
				guid = generateGuid()
				tags := executor.Tags{"some-tag": "some-value"}
				allocationRequest = executor.NewAllocationRequest(guid, &executor.Resource{}, tags)
			})

			JustBeforeEach(func() {
				allocationFailures, allocErr = executorClient.AllocateContainers(logger, []executor.AllocationRequest{allocationRequest})
			})

			It("does not return an error", func() {
				Expect(allocErr).NotTo(HaveOccurred())
			})

			It("returns an empty error map", func() {
				Expect(allocationFailures).To(BeEmpty())
			})

			It("shows up in the container list", func() {
				containers, err := executorClient.ListContainers(logger)
				Expect(err).NotTo(HaveOccurred())
				containers = removeHealthcheckContainers(containers)

				Expect(containers).To(HaveLen(1))

				Expect(containers[0].State).To(Equal(executor.StateReserved))
				Expect(containers[0].Guid).To(Equal(guid))
				Expect(containers[0].MemoryMB).To(Equal(0))
				Expect(containers[0].DiskMB).To(Equal(0))
				Expect(containers[0].Tags).To(Equal(executor.Tags{"some-tag": "some-value"}))
				Expect(containers[0].State).To(Equal(executor.StateReserved))
				Expect(containers[0].AllocatedAt).To(BeNumerically("~", time.Now().UnixNano(), time.Second))
			})

			Context("when allocated with memory and disk limits", func() {
				BeforeEach(func() {
					allocationRequest.Resource.MemoryMB = 256
					allocationRequest.Resource.DiskMB = 256
				})

				It("returns the limits on the container", func() {
					containers, err := executorClient.ListContainers(logger)
					Expect(err).NotTo(HaveOccurred())
					containers = removeHealthcheckContainers(containers)

					Expect(containers).To(HaveLen(1))
					Expect(containers[0].MemoryMB).To(Equal(256))
					Expect(containers[0].DiskMB).To(Equal(256))
				})

				It("reduces the capacity by the amount reserved", func() {
					Expect(executorClient.RemainingResources(logger)).To(Equal(executor.ExecutorResources{
						MemoryMB:   int(gardenCapacity.MemoryInBytes/1024/1024) - 256,
						DiskMB:     int(gardenCapacity.DiskInBytes/1024/1024) - 256,
						Containers: int(gardenCapacity.MaxContainers) - 1,
					}))
				})
			})

			Context("when the guid is already taken", func() {
				JustBeforeEach(func() {
					Expect(allocErr).NotTo(HaveOccurred())
					allocationFailures, allocErr = executorClient.AllocateContainers(logger, []executor.AllocationRequest{allocationRequest})
				})

				It("returns an error", func() {
					Expect(allocErr).NotTo(HaveOccurred())
					Expect(allocationFailures).To(HaveLen(1))
					Expect(allocationFailures[0].Error()).To(Equal(executor.ErrContainerGuidNotAvailable.Error()))
				})
			})

			Context("when a guid is not specified", func() {
				BeforeEach(func() {
					allocationRequest.Guid = ""
				})

				It("returns an error", func() {
					Expect(allocErr).NotTo(HaveOccurred())
					Expect(allocationFailures).To(HaveLen(1))
					Expect(allocationFailures[0].Error()).To(Equal(executor.ErrGuidNotSpecified.Error()))
				})
			})

			Context("when there is no room", func() {
				BeforeEach(func() {
					allocationRequest.Resource.MemoryMB = 999999999999999
					allocationRequest.Resource.DiskMB = 999999999999999
				})

				It("returns an error", func() {
					Expect(allocErr).NotTo(HaveOccurred())
					Expect(allocationFailures).To(HaveLen(1))
					Expect(allocationFailures[0].Error()).To(Equal(executor.ErrInsufficientResourcesAvailable.Error()))
				})
			})

			Describe("running it", func() {
				var runErr error
				var runReq executor.RunRequest
				var runInfo executor.RunInfo
				var eventSource executor.EventSource

				BeforeEach(func() {
					runInfo = executor.RunInfo{
						Env: []executor.EnvironmentVariable{
							{Name: "ENV1", Value: "val1"},
							{Name: "ENV2", Value: "val2"},
						},

						Action: models.WrapAction(&models.RunAction{
							Path: "true",
							User: "vcap",
							Env: []*models.EnvironmentVariable{
								{Name: "RUN_ENV1", Value: "run_val1"},
								{Name: "RUN_ENV2", Value: "run_val2"},
							},
						}),
					}
				})

				JustBeforeEach(func() {
					var err error

					eventSource, err = executorClient.SubscribeToEvents(logger)
					Expect(err).NotTo(HaveOccurred())

					runReq = executor.NewRunRequest(guid, &runInfo, executor.Tags{})
					runErr = executorClient.RunContainer(logger, &runReq)
				})

				AfterEach(func() {
					eventSource.Close()
				})

				Context("when the container can be created", func() {
					var gardenContainer garden.Container

					JustBeforeEach(func() {
						gardenContainer = findGardenContainer(guid)
					})

					It("returns no error", func() {
						Expect(runErr).NotTo(HaveOccurred())
					})

					It("creates it with the configured owner", func() {
						info, err := gardenContainer.Info()
						Expect(err).NotTo(HaveOccurred())

						Expect(info.Properties["executor:owner"]).To(Equal(ownerName))
					})

					It("sets global environment variables on the container", func() {
						output := gbytes.NewBuffer()

						process, err := gardenContainer.Run(garden.ProcessSpec{
							Path: "env",
							User: "vcap",
						}, garden.ProcessIO{
							Stdout: output,
						})
						Expect(err).NotTo(HaveOccurred())
						Expect(process.Wait()).To(Equal(0))

						Expect(output.Contents()).To(ContainSubstring("ENV1=val1"))
						Expect(output.Contents()).To(ContainSubstring("ENV2=val2"))
					})

					It("saves the succeeded run result", func() {
						Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCompleted))

						container := getContainer(guid)
						Expect(container.RunResult.Failed).To(BeFalse())
						Expect(container.RunResult.FailureReason).To(BeEmpty())
					})

					Context("when created without a monitor action", func() {
						BeforeEach(func() {
							runInfo.Action = models.WrapAction(&models.RunAction{
								Path: "sh",
								User: "vcap",
								Args: []string{"-c", "while true; do sleep 1; done"},
							})
						})

						It("reports the state as 'running'", func() {
							Eventually(containerStatePoller(guid)).Should(Equal(executor.StateRunning))
							Consistently(containerStatePoller(guid)).Should(Equal(executor.StateRunning))
						})
					})

					Context("when created with a monitor action", func() {
						itFailsOnlyIfMonitoringSucceedsAndThenFails := func() {
							Context("when monitoring succeeds", func() {
								BeforeEach(func() {
									runInfo.Monitor = models.WrapAction(&models.RunAction{
										User: "vcap",
										Path: "true",
									})
								})

								It("emits a running container event", func() {
									var event executor.Event
									Eventually(containerEventPoller(eventSource, &event), 5).Should(Equal(executor.EventTypeContainerRunning))
								})

								It("reports the state as 'running'", func() {
									Eventually(containerStatePoller(guid)).Should(Equal(executor.StateRunning))
									Consistently(containerStatePoller(guid)).Should(Equal(executor.StateRunning))
								})

								It("does not stop the container", func() {
									Consistently(containerStatePoller(guid)).ShouldNot(Equal(executor.StateCompleted))
								})
							})

							Context("when monitoring persistently fails", func() {
								BeforeEach(func() {
									runInfo.Monitor = models.WrapAction(&models.RunAction{
										User: "vcap",
										Path: "false",
									})
								})

								It("reports the state as 'created'", func() {
									Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCreated))
									Consistently(containerStatePoller(guid)).Should(Equal(executor.StateCreated))
								})
							})

							Context("when monitoring succeeds and then fails", func() {
								BeforeEach(func() {
									runInfo.Monitor = models.WrapAction(&models.RunAction{
										User: "vcap",
										Path: "sh",
										Args: []string{
											"-c",
											`
													if [ -f already_ran ]; then
														exit 1
													else
														touch already_ran
													fi
												`,
										},
									})
								})

								It("reports the container as 'running' and then as 'completed'", func() {
									Eventually(containerStatePoller(guid)).Should(Equal(executor.StateRunning))
									Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCompleted))
								})
							})
						}

						Context("when the action succeeds and exits immediately (daemonization)", func() {
							BeforeEach(func() {
								runInfo.Action = models.WrapAction(&models.RunAction{
									Path: "true",
									User: "vcap",
								})
							})

							itFailsOnlyIfMonitoringSucceedsAndThenFails()
						})

						Context("while the action does not stop running", func() {
							BeforeEach(func() {
								runInfo.Action = models.WrapAction(&models.RunAction{
									Path: "sh",
									User: "vcap",
									Args: []string{"-c", "while true; do sleep 1; done"},
								})
							})

							itFailsOnlyIfMonitoringSucceedsAndThenFails()
						})

						Context("when the action fails", func() {
							BeforeEach(func() {
								runInfo.Action = models.WrapAction(&models.RunAction{
									User: "vcap",
									Path: "false",
								})
							})

							Context("even if the monitoring succeeds", func() {
								BeforeEach(func() {
									runInfo.Monitor = models.WrapAction(&models.RunAction{
										User: "vcap",
										Path: "true",
									})
								})

								It("stops the container", func() {
									Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCompleted))
								})
							})
						})
					})

					Context("after running succeeds", func() {
						Describe("deleting the container", func() {
							It("works", func(done Done) {
								defer close(done)

								Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCompleted))

								err := executorClient.DeleteContainer(logger, guid)
								Expect(err).NotTo(HaveOccurred())
							}, 5)
						})
					})

					Context("when running fails", func() {
						BeforeEach(func() {
							runInfo.Action = models.WrapAction(&models.RunAction{
								User: "vcap",
								Path: "false",
							})
						})

						It("saves the failed result and reason", func() {
							Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCompleted))

							container := getContainer(guid)
							Expect(container.RunResult.Failed).To(BeTrue())
							Expect(container.RunResult.FailureReason).To(Equal("Exited with status 1"))
						})

						Context("when listening for events", func() {
							It("emits a completed container event", func() {
								var event executor.Event
								Eventually(containerEventPoller(eventSource, &event), 5).Should(Equal(executor.EventTypeContainerComplete))

								completeEvent := event.(executor.ContainerCompleteEvent)
								Expect(completeEvent.Container().State).To(Equal(executor.StateCompleted))
								Expect(completeEvent.Container().RunResult.Failed).To(BeTrue())
								Expect(completeEvent.Container().RunResult.FailureReason).To(Equal("Exited with status 1"))
							})
						})
					})
				})

				Context("when the container cannot be created", func() {
					BeforeEach(func() {
						allocationRequest.RootFSPath = "gopher://example.com"
					})

					It("does not immediately return an error", func() {
						Expect(runErr).NotTo(HaveOccurred())
					})

					Context("when listening for events", func() {
						It("eventually completes with failure", func() {
							Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCompleted))

							container := getContainer(guid)
							Expect(container.RunResult.Failed).To(BeTrue())
							Expect(container.RunResult.FailureReason).To(Equal("failed to initialize container"))
						})
					})
				})
			})
		})

		Describe("running a bogus guid", func() {
			It("returns an error", func() {
				runReq := executor.NewRunRequest("bogus", &executor.RunInfo{}, executor.Tags{})
				err := executorClient.RunContainer(logger, &runReq)
				Expect(err).To(Equal(executor.ErrContainerNotFound))
			})
		})

		Context("when the container has been allocated", func() {
			var guid string

			JustBeforeEach(func() {
				guid = allocNewContainer(executor.Container{
					Resource: executor.Resource{
						MemoryMB: 1024,
						DiskMB:   1024,
					},
				})
			})

			Describe("deleting it", func() {
				It("makes the previously allocated resources available again", func() {
					err := executorClient.DeleteContainer(logger, guid)
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() (executor.ExecutorResources, error) { return executorClient.RemainingResources(logger) }).Should(Equal(executor.ExecutorResources{
						MemoryMB:   int(gardenCapacity.MemoryInBytes / 1024 / 1024),
						DiskMB:     int(gardenCapacity.DiskInBytes / 1024 / 1024),
						Containers: int(gardenCapacity.MaxContainers),
					}))
				})
			})

			Describe("listing containers", func() {
				It("shows up in the container list in reserved state", func() {
					containers, err := executorClient.ListContainers(logger)
					Expect(err).NotTo(HaveOccurred())
					containers = removeHealthcheckContainers(containers)

					Expect(containers).To(HaveLen(1))
					Expect(containers[0].Guid).To(Equal(guid))
					Expect(containers[0].State).To(Equal(executor.StateReserved))
				})
			})
		})

		Context("while it is running", func() {
			var guid string

			JustBeforeEach(func() {
				guid = allocNewContainer(executor.Container{
					Resource: executor.Resource{
						MemoryMB: 64,
						DiskMB:   64,
					},
				})

				runRequest := executor.NewRunRequest(guid, &executor.RunInfo{
					Action: models.WrapAction(&models.RunAction{
						Path: "sh",
						User: "vcap",
						Args: []string{"-c", "while true; do sleep 1; done"},
					}),
				}, executor.Tags{})

				err := executorClient.RunContainer(logger, &runRequest)
				Expect(err).NotTo(HaveOccurred())

				Eventually(containerStatePoller(guid)).Should(Equal(executor.StateRunning))
			})

			Describe("StopContainer", func() {
				It("does not return an error", func() {
					err := executorClient.StopContainer(logger, guid)
					Expect(err).NotTo(HaveOccurred())
				})

				It("stops the container but does not delete it", func() {
					err := executorClient.StopContainer(logger, guid)
					Expect(err).NotTo(HaveOccurred())

					var container executor.Container
					Eventually(func() executor.State {
						container, err = executorClient.GetContainer(logger, guid)
						Expect(err).NotTo(HaveOccurred())
						return container.State
					}).Should(Equal(executor.StateCompleted))

					Expect(container.RunResult.Stopped).To(BeTrue())

					gardenContainer, err := gardenClient.Lookup(guid)
					Expect(err).NotTo(HaveOccurred())

					info, err := gardenContainer.Info()
					Expect(err).NotTo(HaveOccurred())
					Expect(info.State).To(Equal("stopped"))
				})
			})

			Describe("DeleteContainer", func() {
				It("deletes the container", func() {
					err := executorClient.DeleteContainer(logger, guid)
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() error {
						_, err := gardenClient.Lookup(guid)
						return err
					}).Should(HaveOccurred())
				})
			})

			Describe("listing containers", func() {
				It("shows up in the container list in running state", func() {
					containers, err := executorClient.ListContainers(logger)
					Expect(err).NotTo(HaveOccurred())
					containers = removeHealthcheckContainers(containers)

					Expect(containers).To(HaveLen(1))
					Expect(containers[0].Guid).To(Equal(guid))
					Expect(containers[0].State).To(Equal(executor.StateRunning))
				})
			})

			Describe("remaining resources", func() {
				It("has the container's reservation subtracted", func() {
					remaining, err := executorClient.RemainingResources(logger)
					Expect(err).NotTo(HaveOccurred())

					Expect(remaining.MemoryMB).To(Equal(int(gardenCapacity.MemoryInBytes/1024/1024) - 64))
					Expect(remaining.DiskMB).To(Equal(int(gardenCapacity.DiskInBytes/1024/1024) - 64))
				})

				Context("when the container disappears", func() {
					It("eventually goes back to the total resources", func() {
						// wait for the container to be present
						findGardenContainer(guid)

						// delete it
						err := executorClient.DeleteContainer(logger, guid)
						Expect(err).NotTo(HaveOccurred())

						Eventually(func() (executor.ExecutorResources, error) { return executorClient.RemainingResources(logger) }).Should(
							Equal(executor.ExecutorResources{
								MemoryMB:   int(gardenCapacity.MemoryInBytes / 1024 / 1024),
								DiskMB:     int(gardenCapacity.DiskInBytes / 1024 / 1024),
								Containers: int(gardenCapacity.MaxContainers),
							}))
					})
				})
			})

			Describe("removing the container from garden", func() {
				It("transistions the container into the completed state", func() {
					findGardenContainer(guid)

					// delete it
					err := gardenClient.Destroy(guid)
					Expect(err).NotTo(HaveOccurred())

					Eventually(containerStatePoller(guid)).Should(Equal(executor.StateCompleted))
					container := getContainer(guid)
					Expect(container.RunResult.Failed).To(BeTrue())
				})
			})
		})

		Describe("getting files from a container", func() {
			var (
				guid string

				stream    io.ReadCloser
				streamErr error
			)

			Context("when the container hasn't been initialized", func() {
				JustBeforeEach(func() {
					guid = allocNewContainer(executor.Container{
						Resource: executor.Resource{
							MemoryMB: 1024,
							DiskMB:   1024,
						},
					})

					stream, streamErr = executorClient.GetFiles(logger, guid, "some/path")
				})

				It("returns an error", func() {
					Expect(streamErr).To(HaveOccurred())
				})
			})

			Context("when the container is running", func() {
				var container garden.Container

				JustBeforeEach(func() {
					guid = allocNewContainer(executor.Container{})

					runRequest := executor.NewRunRequest(guid, &executor.RunInfo{
						Action: models.WrapAction(&models.RunAction{
							Path: "sh",
							User: "vcap",
							Args: []string{
								"-c", `while true; do	sleep 1; done`,
							},
						}),
					}, executor.Tags{})

					err := executorClient.RunContainer(logger, &runRequest)
					Expect(err).NotTo(HaveOccurred())

					container = findGardenContainer(guid)

					process, err := container.Run(garden.ProcessSpec{
						Path: "sh",
						Args: []string{"-c", "mkdir some; echo hello > some/path"},
						User: "vcap",
					}, garden.ProcessIO{})
					Expect(err).NotTo(HaveOccurred())
					Expect(process.Wait()).To(Equal(0))

					stream, streamErr = executorClient.GetFiles(logger, guid, "/home/vcap/some/path")
				})

				It("does not error", func() {
					Expect(streamErr).NotTo(HaveOccurred())
				})

				It("returns a stream of the contents of the file", func() {
					tarReader := tar.NewReader(stream)

					header, err := tarReader.Next()
					Expect(err).NotTo(HaveOccurred())

					Expect(header.FileInfo().Name()).To(Equal("path"))
					Expect(ioutil.ReadAll(tarReader)).To(Equal([]byte("hello\n")))
				})
			})
		})

		Describe("pruning the registry", func() {
			It("continously prunes the registry", func() {
				_, err := executorClient.AllocateContainers(logger, []executor.AllocationRequest{{
					Guid: "some-handle",
					Resource: executor.Resource{
						MemoryMB: 1024,
						DiskMB:   1024,
					}},
				})
				Expect(err).NotTo(HaveOccurred())

				containers, err := executorClient.ListContainers(logger)
				Expect(err).NotTo(HaveOccurred())
				containers = removeHealthcheckContainers(containers)
				Expect(containers).To(HaveLen(1))

				Eventually(func() executor.State {
					container, err := executorClient.GetContainer(logger, "some-handle")
					Expect(err).NotTo(HaveOccurred())

					return container.State
				}, pruningInterval*3).Should(Equal(executor.StateCompleted))
			})
		})

		Describe("listing containers", func() {
			Context("with no containers", func() {
				It("returns an empty set of containers", func() {
					containers, err := executorClient.ListContainers(logger)
					Expect(err).NotTo(HaveOccurred())
					containers = removeHealthcheckContainers(containers)
					Expect(containers).To(BeEmpty())
				})
			})

			Context("when a container has been allocated", func() {
				var (
					container executor.Container

					guid string
				)

				JustBeforeEach(func() {
					guid = allocNewContainer(container)
				})

				It("includes the allocated container", func() {
					containers, err := executorClient.ListContainers(logger)
					Expect(err).NotTo(HaveOccurred())
					containers = removeHealthcheckContainers(containers)
					Expect(containers).To(HaveLen(1))
					Expect(containers[0].Guid).To(Equal(guid))
				})
			})
		})

		Describe("container networking", func() {
			Context("when a container listens on the local end of CF_INSTANCE_ADDR", func() {
				var guid string
				var containerResponse []byte
				var externalAddr string

				JustBeforeEach(func() {
					guid = allocNewContainer(executor.Container{})
					runRequest := executor.NewRunRequest(guid, &executor.RunInfo{
						Ports: []executor.PortMapping{
							{ContainerPort: 8080},
						},

						Action: models.WrapAction(&models.RunAction{
							User: "vcap",
							Path: "sh",
							Args: []string{"-c", "echo -n .$CF_INSTANCE_ADDR. | nc -l 8080"},
						}),
					}, executor.Tags{})

					err := executorClient.RunContainer(logger, &runRequest)
					Expect(err).NotTo(HaveOccurred())

					Eventually(containerStatePoller(guid)).Should(Equal(executor.StateRunning))

					container := getContainer(guid)

					externalAddr = fmt.Sprintf("%s:%d", container.ExternalIP, container.Ports[0].HostPort)

					var conn net.Conn
					Eventually(func() error {
						var err error
						conn, err = net.Dial("tcp", externalAddr)
						return err
					}).ShouldNot(HaveOccurred())

					containerResponse, err = ioutil.ReadAll(conn)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("when exportNetworkEnvVars is set", func() {
					BeforeEach(func() {
						exportNetworkEnvVars = true
					})

					It("echoes back the correct CF_INSTANCE_ADDR", func() {
						Expect(string(containerResponse)).To(Equal("." + externalAddr + "."))
					})
				})

				Context("when exportNetworkEnvVars is not set", func() {
					BeforeEach(func() {
						exportNetworkEnvVars = false
					})

					It("echoes back an empty CF_INSTANCE_ADDR", func() {
						Expect(string(containerResponse)).To(Equal(".."))
					})
				})
			})
		})
	})
})
