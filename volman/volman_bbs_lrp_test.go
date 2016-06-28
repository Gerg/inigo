package volman_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"code.cloudfoundry.org/auction/auctiontypes"
	"code.cloudfoundry.org/bbs"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/inigo/helpers"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	"code.cloudfoundry.org/inigo/fixtures"
	archive_helper "github.com/pivotal-golang/archiver/extractor/test_helper"
)

var _ = Describe("LRPs with volume mounts", func() {
	var (
		cellProcess         ifrit.Process
		fileServerStaticDir string
		plumbing            ifrit.Process
		logger              lager.Logger
		bbsClient           bbs.InternalClient
		processGuid         string
		archiveFiles        []archive_helper.ArchiveFile
	)

	BeforeEach(func() {
		var fileServerRunner ifrit.Runner
		fileServerRunner, fileServerStaticDir = componentMaker.FileServer()
		plumbing = ginkgomon.Invoke(grouper.NewOrdered(os.Kill, grouper.Members{
			{"initial-services", grouper.NewParallel(os.Kill, grouper.Members{
				{"etcd", componentMaker.Etcd()},
				{"sql", componentMaker.SQL()},
				{"nats", componentMaker.NATS()},
				{"consul", componentMaker.Consul()},
			})},
			{"bbs", componentMaker.BBS()},
		}))

		helpers.ConsulWaitUntilReady()
		logger = lager.NewLogger("test")
		logger.RegisterSink(lager.NewWriterSink(GinkgoWriter, lager.DEBUG))

		cellProcess = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"router", componentMaker.Router()},
			{"file-server", fileServerRunner},
			{"rep", componentMaker.Rep()},
			{"auctioneer", componentMaker.Auctioneer()},
			{"route-emitter", componentMaker.RouteEmitter()},
		}))

		bbsServiceClient := componentMaker.BBSServiceClient(logger)
		bbsClient = componentMaker.BBSClient()
		archiveFiles = fixtures.GoServerApp()

		Eventually(func() (models.CellSet, error) { return bbsServiceClient.Cells(logger) }).Should(HaveLen(1))
	})

	JustBeforeEach(func() {
		archive_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			archiveFiles,
		)
	})

	AfterEach(func() {
		destroyContainerErrors := helpers.CleanupGarden(gardenClient)
		helpers.StopProcesses(plumbing, cellProcess)
		Expect(destroyContainerErrors).To(
			BeEmpty(),
			"%d containers failed to be destroyed!",
			len(destroyContainerErrors),
		)
	})

	Describe("desiring with volume mount", func() {
		var lrp *models.DesiredLRP

		BeforeEach(func() {
			processGuid = helpers.GenerateGuid()

			volumeId := fmt.Sprintf("some-volumeID-%d", time.Now().UnixNano())
			someConfig := map[string]interface{}{"volume_id": volumeId}
			jsonSomeConfig, err := json.Marshal(someConfig)
			Expect(err).NotTo(HaveOccurred())

			lrp = helpers.DefaultLRPCreateRequest(processGuid, "log-guid", 1)

			lrp.VolumeMounts = []*models.VolumeMount{
				&models.VolumeMount{
					Driver:        "localdriver",
					VolumeId:      volumeId,
					ContainerPath: "/testmount",
					Mode:          models.BindMountMode_RW,
					Config:        jsonSomeConfig,
				},
			}

			lrp.CachedDependencies = []*models.CachedDependency{{
				From:      fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "lrp.zip"),
				To:        "/tmp/diego/lrp",
				Name:      "lrp bits",
				CacheKey:  "lrp-cache-key",
				LogSource: "APP",
			}}

			lrp.Privileged = true

			lrp.LegacyDownloadUser = "vcap"
			lrp.Action = models.WrapAction(&models.RunAction{
				User: "vcap",
				Path: "/tmp/diego/lrp/go-server",
				Env: []*models.EnvironmentVariable{
					{"PORT", "8080"},
					{"MOUNT_POINT_DIR", "/testmount"},
				},
			})
		})

		JustBeforeEach(func() {
			err := bbsClient.DesireLRP(logger, lrp)
			Expect(err).NotTo(HaveOccurred())
		})

		It("can write to a file on the mounted volume", func() {
			Eventually(helpers.LRPStatePoller(logger, bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))
			Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(ConsistOf([]string{"0"}))
			body, statusCode, err := helpers.ResponseBodyAndStatusCodeFromHost(componentMaker.Addresses.Router, helpers.DefaultHost, "write")
			Expect(err).NotTo(HaveOccurred())
			Expect(statusCode).To(Equal(200))
			Expect(string(body)).To(Equal("Hello Persistant World!\n"))
		})

		Context("when driver required not available on any cell", func() {
			BeforeEach(func() {
				lrp.VolumeMounts = []*models.VolumeMount{
					generateVolumeObject("non-existent-driver"),
				}
			})

			It("should error placing the lrp", func() {
				var actualLRP *models.ActualLRP
				Eventually(func() interface{} {
					group, err := bbsClient.ActualLRPGroupByProcessGuidAndIndex(logger, processGuid, 0)
					Expect(err).NotTo(HaveOccurred())

					var evacuating bool
					actualLRP, evacuating = group.Resolve()
					Expect(evacuating).To(BeFalse())

					return actualLRP.PlacementError
				}).Should(Equal(auctiontypes.ErrorCellMismatch.Error()))
			})
		})

		Context("when one of the drivers required is not available on any cell", func() {
			BeforeEach(func() {
				lrp.VolumeMounts = []*models.VolumeMount{
					generateVolumeObject("non-existent-driver"),
					generateVolumeObject("localdriver"),
				}
			})

			It("should error placing the task", func() {
				var actualLRP *models.ActualLRP
				Eventually(func() interface{} {
					group, err := bbsClient.ActualLRPGroupByProcessGuidAndIndex(logger, processGuid, 0)
					Expect(err).NotTo(HaveOccurred())

					var evacuating bool
					actualLRP, evacuating = group.Resolve()
					Expect(evacuating).To(BeFalse())

					return actualLRP.PlacementError
				}).Should(Equal(auctiontypes.ErrorCellMismatch.Error()))
			})
		})
	})
})