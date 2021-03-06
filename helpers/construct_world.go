package helpers

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cloudfoundry-incubator/consuladapter/consulrunner"
	"github.com/cloudfoundry-incubator/diego-ssh/keys"
	"github.com/cloudfoundry-incubator/inigo/world"

	"github.com/onsi/ginkgo/config"

	. "github.com/onsi/gomega"
)

var PreloadedStacks = []string{"red-stack", "blue-stack"}
var DefaultStack = PreloadedStacks[0]

var addresses world.ComponentAddresses

const assetsPath = "../fixtures/certs/"

func MakeComponentMaker(builtArtifacts world.BuiltArtifacts, localIP string) world.ComponentMaker {
	gardenBinPath := os.Getenv("GARDEN_BINPATH")
	gardenRootFSPath := os.Getenv("GARDEN_ROOTFS")
	gardenGraphPath := os.Getenv("GARDEN_GRAPH_PATH")
	externalAddress := os.Getenv("EXTERNAL_ADDRESS")

	if gardenGraphPath == "" {
		gardenGraphPath = os.TempDir()
	}

	Expect(gardenBinPath).NotTo(BeEmpty(), "must provide $GARDEN_BINPATH")
	Expect(gardenRootFSPath).NotTo(BeEmpty(), "must provide $GARDEN_ROOTFS")
	Expect(externalAddress).NotTo(BeEmpty(), "must provide $EXTERNAL_ADDRESS")

	stackPathMap := map[string]string{}
	for _, stack := range PreloadedStacks {
		stackPathMap[stack] = gardenRootFSPath
	}

	addresses = world.ComponentAddresses{
		GardenLinux:         fmt.Sprintf("127.0.0.1:%d", 10000+config.GinkgoConfig.ParallelNode),
		NATS:                fmt.Sprintf("127.0.0.1:%d", 11000+config.GinkgoConfig.ParallelNode),
		Etcd:                fmt.Sprintf("127.0.0.1:%d", 12000+config.GinkgoConfig.ParallelNode),
		EtcdPeer:            fmt.Sprintf("127.0.0.1:%d", 12500+config.GinkgoConfig.ParallelNode),
		Consul:              fmt.Sprintf("127.0.0.1:%d", 12750+config.GinkgoConfig.ParallelNode*consulrunner.PortOffsetLength),
		Rep:                 fmt.Sprintf("0.0.0.0:%d", 14000+config.GinkgoConfig.ParallelNode),
		FileServer:          fmt.Sprintf("%s:%d", localIP, 17000+config.GinkgoConfig.ParallelNode),
		Router:              fmt.Sprintf("127.0.0.1:%d", 18000+config.GinkgoConfig.ParallelNode),
		TPSListener:         fmt.Sprintf("127.0.0.1:%d", 19000+config.GinkgoConfig.ParallelNode),
		FakeCC:              fmt.Sprintf("127.0.0.1:%d", 20000+config.GinkgoConfig.ParallelNode),
		BBS:                 fmt.Sprintf("127.0.0.1:%d", 20500+config.GinkgoConfig.ParallelNode),
		Receptor:            fmt.Sprintf("127.0.0.1:%d", 21000+config.GinkgoConfig.ParallelNode),
		ReceptorTaskHandler: fmt.Sprintf("127.0.0.1:%d", 21500+config.GinkgoConfig.ParallelNode),
		Stager:              fmt.Sprintf("127.0.0.1:%d", 22000+config.GinkgoConfig.ParallelNode),
		NsyncListener:       fmt.Sprintf("127.0.0.1:%d", 22500+config.GinkgoConfig.ParallelNode),
		Auctioneer:          fmt.Sprintf("0.0.0.0:%d", 23000+config.GinkgoConfig.ParallelNode),
		SSHProxy:            fmt.Sprintf("127.0.0.1:%d", 23500+config.GinkgoConfig.ParallelNode),
	}

	hostKeyPair, err := keys.RSAKeyPairFactory.NewKeyPair(1024)
	Expect(err).NotTo(HaveOccurred())

	userKeyPair, err := keys.RSAKeyPairFactory.NewKeyPair(1024)
	Expect(err).NotTo(HaveOccurred())

	sshKeys := world.SSHKeys{
		HostKey:       hostKeyPair.PrivateKey(),
		HostKeyPem:    hostKeyPair.PEMEncodedPrivateKey(),
		PrivateKeyPem: userKeyPair.PEMEncodedPrivateKey(),
		AuthorizedKey: userKeyPair.AuthorizedKey(),
	}
	serverCert, err := filepath.Abs(assetsPath + "server.crt")
	Expect(err).NotTo(HaveOccurred())
	serverKey, err := filepath.Abs(assetsPath + "server.key")
	Expect(err).NotTo(HaveOccurred())
	clientCrt, err := filepath.Abs(assetsPath + "client.crt")
	Expect(err).NotTo(HaveOccurred())
	clientKey, err := filepath.Abs(assetsPath + "client.key")
	Expect(err).NotTo(HaveOccurred())
	caCert, err := filepath.Abs(assetsPath + "ca.crt")
	Expect(err).NotTo(HaveOccurred())

	sslConfig := world.SSLConfig{
		ServerCert: serverCert,
		ServerKey:  serverKey,
		ClientCert: clientCrt,
		ClientKey:  clientKey,
		CACert:     caCert,
	}

	return world.ComponentMaker{
		Artifacts: builtArtifacts,
		Addresses: addresses,

		PreloadedStackPathMap: stackPathMap,

		ExternalAddress: externalAddress,

		GardenBinPath:   gardenBinPath,
		GardenGraphPath: gardenGraphPath,
		SSHConfig:       sshKeys,
		SSL:             sslConfig,
	}
}
