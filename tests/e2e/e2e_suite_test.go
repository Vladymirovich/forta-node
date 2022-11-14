package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/forta-network/forta-core-go/contracts/contract_access_manager"
	"github.com/forta-network/forta-core-go/contracts/contract_agent_registry"
	"github.com/forta-network/forta-core-go/contracts/contract_dispatch"
	"github.com/forta-network/forta-core-go/contracts/contract_forta_staking"
	"github.com/forta-network/forta-core-go/contracts/contract_router"
	"github.com/forta-network/forta-core-go/contracts/contract_scanner_node_version"
	"github.com/forta-network/forta-core-go/contracts/contract_scanner_registry"
	"github.com/forta-network/forta-core-go/ens"
	"github.com/forta-network/forta-core-go/manifest"
	"github.com/forta-network/forta-core-go/release"
	"github.com/forta-network/forta-core-go/utils"
	"github.com/forta-network/forta-node/clients"
	"github.com/forta-network/forta-node/config"
	"github.com/forta-network/forta-node/tests/e2e/ethaccounts"
	"github.com/forta-network/forta-node/tests/e2e/misccontracts/contract_erc20"
	"github.com/forta-network/forta-node/tests/e2e/misccontracts/contract_transparent_upgradeable_proxy"
	"github.com/forta-network/forta-node/testutils/alertserver"
	ipfsapi "github.com/ipfs/go-ipfs-api"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

const (
	smallTimeout = time.Minute * 3
	largeTimeout = time.Minute * 5
)

var (
	ethereumDataDir         = ".ethereum"
	ipfsDataDir             = ".ipfs"
	genesisFile             = "genesis.json"
	passwordFile            = "ethaccounts/password"
	gethKeyFile             = "ethaccounts/gethkeyfile"
	networkID               = int64(137)
	gethNodeEndpoint        = "http://localhost:8545"
	processStartWaitSeconds = 30
	txWaitSeconds           = 5
	ipfsEndpoint            = "http://localhost:5002"
	discoConfigFile         = "disco.config.yml"
	discoPort               = "1970"

	agentID         = "0x8fe07f1a4d33b30be2387293f052c273660c829e9a6965cf7e8d485bcb871083"
	agentIDBigInt   = utils.AgentHexToBigInt(agentID)
	scannerIDBigInt = utils.ScannerIDHexToBigInt(ethaccounts.ScannerAddress.Hex())
	// to be set in forta-agent-0x04f4b6-02b4 format
	agentContainerID string

	runnerSupervisedContainers = []string{
		"forta-updater",
		"forta-supervisor",
	}

	allServiceContainers = []string{
		"forta-updater",
		"forta-supervisor",
		"forta-json-rpc",
		"forta-scanner",
		"forta-nats",
	}

	envEmptyRunnerTrackingID = "RUNNER_TRACKING_ID="

	testAgentLocalImageName = "forta-e2e-test-agent"
)

type Suite struct {
	ctx context.Context
	r   *require.Assertions

	alertServer *alertserver.AlertServer

	ipfsClient   *ipfsapi.Shell
	ethClient    *ethclient.Client
	dockerClient clients.DockerClient

	deployer *bind.TransactOpts
	admin    *bind.TransactOpts
	scanner  *bind.TransactOpts

	tokenContract          *contract_erc20.ERC20
	stakingContract        *contract_forta_staking.FortaStaking
	scannerRegContract     *contract_scanner_registry.ScannerRegistry
	agentRegContract       *contract_agent_registry.AgentRegistry
	dispatchContract       *contract_dispatch.Dispatch
	scannerVersionContract *contract_scanner_node_version.ScannerNodeVersion

	releaseManifest    *release.ReleaseManifest
	releaseManifestCid string

	agentManifest    *manifest.SignedAgentManifest
	agentManifestCid string

	fortaProcess *Process

	suite.Suite
}

func TestE2E(t *testing.T) {
	if os.Getenv("E2E_TEST") != "1" {
		t.Log("e2e testing is not enabled (skipping) - enable with E2E_TEST=1 env var")
		return
	}

	s := &Suite{
		ctx: context.Background(),
		r:   require.New(t),
	}
	dockerClient, err := clients.NewDockerClient("")
	s.r.NoError(err)
	s.dockerClient = dockerClient

	s.ipfsClient = ipfsapi.NewShell(ipfsEndpoint)
	s.ensureAvailability("ipfs", func() error {
		_, err := s.ipfsClient.FilesLs(s.ctx, "/")
		if err != nil {
			return err
		}
		return nil
	})

	s.ensureAvailability("disco", func() error {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/v2/", discoPort))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		return fmt.Errorf("disco responded with status '%d'", resp.StatusCode)
	})

	ethClient, err := ethclient.Dial(gethNodeEndpoint)
	s.r.NoError(err)
	s.ethClient = ethClient
	s.ensureAvailability("geth", func() error {
		_, err := ethClient.BlockNumber(s.ctx)
		return err
	})

	suite.Run(t, s)
}

func (s *Suite) SetupTest() {
	s.ctx = context.Background()
	s.r = require.New(s.T())

	s.deployer = bind.NewKeyedTransactor(ethaccounts.DeployerKey)
	s.admin = bind.NewKeyedTransactor(ethaccounts.AccessAdminKey)
	s.scanner = bind.NewKeyedTransactor(ethaccounts.ScannerKey)

	accessMgrAddr, err := s.deployContractWithProxy(
		"AccessManager", s.deployer, contract_access_manager.AccessManagerMetaData,
	)
	s.r.NoError(err)
	accessMgrContract, _ := contract_access_manager.NewAccessManager(accessMgrAddr, s.ethClient)
	tx, err := accessMgrContract.Initialize(s.deployer, ethaccounts.AccessAdminAddress)
	s.r.NoError(err)
	s.ensureTx("AccessManager.initialize()", tx)

	// give role permissions to manager account

	roleDefaultAdmin := common.Hash{}
	s.T().Logf("DEFAULT_ADMIN_ROLE: %s", roleDefaultAdmin.Hex())
	roleScannerVersion := crypto.Keccak256Hash([]byte("SCANNER_VERSION_ROLE"))
	s.T().Logf("SCANNER_VERSION_ROLE: %s", roleScannerVersion.Hex())
	roleDispatcher := crypto.Keccak256Hash([]byte("DISPATCHER_ROLE"))
	s.T().Logf("DISPATCHER_ROLE: %s", roleDispatcher.Hex())
	roleScannerAdmin := crypto.Keccak256Hash([]byte("SCANNER_ADMIN_ROLE"))
	s.T().Logf("SCANNER_ADMIN_ROLE: %s", roleScannerAdmin.Hex())

	hasRole, err := accessMgrContract.HasRole(&bind.CallOpts{From: ethaccounts.AccessAdminAddress}, roleDefaultAdmin, ethaccounts.AccessAdminAddress)
	s.r.NoError(err)
	s.T().Log("admin has role default:", hasRole)

	tx, err = accessMgrContract.SetNewRole(s.admin, roleScannerVersion, roleDefaultAdmin)
	s.r.NoError(err)
	s.ensureTx("AccessManager set SCANNER_VERSION_ROLE", tx)

	tx, err = accessMgrContract.SetNewRole(s.admin, roleDispatcher, roleDefaultAdmin)
	s.r.NoError(err)
	s.ensureTx("AccessManager set DISPATCHER_ROLE", tx)

	tx, err = accessMgrContract.SetNewRole(s.admin, roleScannerAdmin, roleDefaultAdmin)
	s.r.NoError(err)
	s.ensureTx("AccessManager set SCANNER_ADMIN_ROLE", tx)

	tx, err = accessMgrContract.GrantRole(
		s.admin, roleScannerVersion, ethaccounts.AccessAdminAddress,
	)
	s.r.NoError(err)
	s.ensureTx("AccessManager grant SCANNER_VERSION_ROLE to admin", tx)

	tx, err = accessMgrContract.GrantRole(
		s.admin, roleDispatcher, ethaccounts.AccessAdminAddress,
	)
	s.r.NoError(err)
	s.ensureTx("AccessManager grant DISPATCHER_ROLE to admin", tx)

	tx, err = accessMgrContract.GrantRole(
		s.admin, roleScannerAdmin, ethaccounts.AccessAdminAddress,
	)
	s.r.NoError(err)
	s.ensureTx("AccessManager grant SCANNER_ADMIN_ROLE to admin", tx)

	routerAddr, err := s.deployContractWithProxy(
		"Router", s.deployer, contract_router.RouterMetaData,
	)
	s.r.NoError(err)
	routerContract, _ := contract_router.NewRouter(routerAddr, s.ethClient)
	tx, err = routerContract.Initialize(s.deployer, accessMgrAddr)
	s.r.NoError(err)
	s.ensureTx("Router.initialize()", tx)

	tokenAddr, tx, tokenContract, err := contract_erc20.DeployERC20(s.deployer, s.ethClient, "FORT", "FORT")
	s.r.NoError(err)
	s.ensureTx("ERC20 (FORT) deployment", tx)
	s.tokenContract = tokenContract

	stakingAddr, err := s.deployContractWithProxy(
		"FortaStaking", s.deployer, contract_forta_staking.FortaStakingMetaData,
	)
	s.r.NoError(err)
	stakingContract, _ := contract_forta_staking.NewFortaStaking(stakingAddr, s.ethClient)
	s.stakingContract = stakingContract
	tx, err = stakingContract.Initialize(s.deployer, accessMgrAddr, routerAddr, tokenAddr, 0, ethaccounts.MiscAddress)
	s.r.NoError(err)
	s.ensureTx("FortaStaking.initialize()", tx)

	scannerRegAddr, err := s.deployContractWithProxy(
		"ScannerRegistry", s.deployer, contract_scanner_registry.ScannerRegistryMetaData,
	)
	s.r.NoError(err)
	scannerRegContract, _ := contract_scanner_registry.NewScannerRegistry(scannerRegAddr, s.ethClient)
	s.scannerRegContract = scannerRegContract
	tx, err = scannerRegContract.Initialize(s.deployer, accessMgrAddr, routerAddr, "Forta Scanners", "FScanners")
	s.r.NoError(err)
	s.ensureTx("ScannerRegistry.initialize()", tx)

	// set stake threshold as zero for now
	tx, err = scannerRegContract.SetStakeThreshold(
		s.admin, contract_scanner_registry.IStakeSubjectStakeThreshold{
			Min:       big.NewInt(0),
			Max:       big.NewInt(1),
			Activated: true,
		}, big.NewInt(networkID))
	s.r.NoError(err)
	s.ensureTx("ScannerRegistry.setStakeThreshold()", tx)

	agentRegAddr, err := s.deployContractWithProxy(
		"ScannerRegistry", s.deployer, contract_agent_registry.AgentRegistryMetaData,
	)
	s.r.NoError(err)
	agentRegContract, _ := contract_agent_registry.NewAgentRegistry(agentRegAddr, s.ethClient)
	s.agentRegContract = agentRegContract
	tx, err = agentRegContract.Initialize(s.deployer, accessMgrAddr, routerAddr, "Forta Agents", "FAgents")
	s.r.NoError(err)
	s.ensureTx("AgentRegistry.initialize()", tx)

	dispatchAddr, err := s.deployContractWithProxy(
		"ScannerRegistry", s.deployer, contract_dispatch.DispatchMetaData,
	)
	s.r.NoError(err)
	dispatchRegContract, _ := contract_dispatch.NewDispatch(dispatchAddr, s.ethClient)
	s.dispatchContract = dispatchRegContract
	tx, err = dispatchRegContract.Initialize(s.deployer, accessMgrAddr, routerAddr, agentRegAddr, scannerRegAddr)
	s.r.NoError(err)
	s.ensureTx("Dispatch.initialize()", tx)

	scannerVersionAddress, err := s.deployContractWithProxy(
		"ScannerNodeVersion", s.deployer, contract_scanner_node_version.ScannerNodeVersionMetaData,
	)
	s.r.NoError(err)
	scannerVersionContract, _ := contract_scanner_node_version.NewScannerNodeVersion(scannerVersionAddress, s.ethClient)
	s.scannerVersionContract = scannerVersionContract
	tx, err = scannerVersionContract.Initialize(s.deployer, accessMgrAddr, routerAddr)
	s.r.NoError(err)
	s.ensureTx("ScannerNodeVersion.initialize()", tx)

	// let deployer be

	ensOverrides := map[string]string{
		ens.DispatchContract:           dispatchAddr.Hex(),
		ens.AgentRegistryContract:      agentRegAddr.Hex(),
		ens.ScannerRegistryContract:    scannerRegAddr.Hex(),
		ens.ScannerNodeVersionContract: scannerVersionAddress.Hex(),
		ens.StakingContract:            stakingAddr.Hex(),
	}
	b, _ := json.MarshalIndent(ensOverrides, "", "  ")
	s.r.NoError(ioutil.WriteFile(".forta/ens-override.json", b, 0644))
	s.r.NoError(ioutil.WriteFile(".forta-local/ens-override.json", b, 0644))

	// set runtime vars and put release to ipfs and to the scanner version contract
	nodeImageRef := s.readImageRef("node")
	config.DockerSupervisorImage = nodeImageRef
	config.DockerUpdaterImage = nodeImageRef
	config.UseDockerImages = "remote"
	config.Version = "0.0.1"
	s.releaseManifest = &release.ReleaseManifest{
		Release: release.Release{
			Timestamp:  time.Now().String(),
			Repository: "https://github.com/forta-network/forta-node",
			Version:    config.Version,
			Commit:     "57f35d25384ddf3f35731c636515204b1757c6ba",
			Services: release.ReleaseServices{
				Updater:    nodeImageRef,
				Supervisor: nodeImageRef,
			},
		},
	}
	s.releaseManifestCid = s.ipfsFilesAdd("/release", s.releaseManifest)
	config.ReleaseCid = s.releaseManifestCid
	tx, err = s.scannerVersionContract.SetScannerNodeVersion(s.admin, s.releaseManifestCid)
	s.r.NoError(err)
	s.ensureTx("ScannerNodeVersion version update", tx)

	// put agent manifest to ipfs
	agentImageRef := s.readImageRef("agent")
	s.agentManifest = &manifest.SignedAgentManifest{
		Manifest: &manifest.AgentManifest{
			From:           utils.StringPtr(ethaccounts.MiscAddress.Hex()),
			Name:           utils.StringPtr("Exploiter Transaction Detector"),
			AgentID:        utils.StringPtr("Exploiter Transaction Detector"),
			AgentIDHash:    utils.StringPtr(agentID),
			Version:        utils.StringPtr("0.0.1"),
			Timestamp:      utils.StringPtr(time.Now().String()),
			ImageReference: utils.StringPtr(agentImageRef),
			Repository:     utils.StringPtr("https://github.com/forta-network/forta-node/tree/master/tests/e2e/agents/txdetectoragent"),
			ChainIDs:       []int64{networkID},
		},
	}
	s.agentManifestCid = s.ipfsFilesAdd("/agent", s.agentManifest)

	agentContainerID = config.AgentConfig{
		ID:    agentID,
		Image: agentImageRef,
	}.ContainerName()

	// register agent
	tx, err = s.agentRegContract.CreateAgent(
		s.admin, agentIDBigInt, ethaccounts.MiscAddress, s.agentManifestCid, []*big.Int{big.NewInt(networkID)},
	)
	s.r.NoError(err)
	s.ensureTx("AgentRegitry.createAgent() - creating exploiter tx detector agent", tx)

	// start the fake alert server
	s.alertServer = alertserver.New(s.ctx, 9090)
	go s.alertServer.Start()
}

func attachCmdOutput(cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stdout
}

func (s *Suite) runCmd(name string, arg ...string) {
	cmd := exec.Command(name, arg...)
	attachCmdOutput(cmd)
	s.r.NoError(cmd.Run())
}

func (s *Suite) runCmdSilent(name string, arg ...string) {
	cmd := exec.Command(name, arg...)
	s.r.NoError(cmd.Run())
}

func (s *Suite) ensureTx(name string, tx *types.Transaction) {
	for i := 0; i < txWaitSeconds*5; i++ {
		receipt, err := s.ethClient.TransactionReceipt(s.ctx, tx.Hash())
		if err == nil {
			s.r.Equal(tx.Hash().Hex(), receipt.TxHash.Hex())
			s.T().Logf("%s - mined: %s", name, tx.Hash())
			return
		}
		time.Sleep(time.Millisecond * 200)
	}
	time.Sleep(time.Second) // hard delay
	s.r.FailNowf("failed to mine tx", "%s: %s", name, tx.Hash())
}

func (s *Suite) deployContractWithProxy(
	name string, auth *bind.TransactOpts, contractMetaData *bind.MetaData,
) (common.Address, error) {
	abi, bin := getAbiAndBin(contractMetaData)
	address, tx, _, err := bind.DeployContract(auth, *abi, common.FromHex(bin), s.ethClient, ethaccounts.ForwarderAddress)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to deploy logic contract: %v", err)
	}
	s.ensureTx(fmt.Sprintf("%s deployment", name), tx)
	proxyAddress, tx, _, err := contract_transparent_upgradeable_proxy.DeployTransparentUpgradeableProxy(
		auth, s.ethClient, address, ethaccounts.ProxyAdminAddress, nil,
	)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to deploy proxy: %v", err)
	}
	s.ensureTx(fmt.Sprintf("%s proxy deployment", name), tx)
	return proxyAddress, nil
}

func getAbiAndBin(metadata *bind.MetaData) (*abi.ABI, string) {
	parsed, _ := metadata.GetAbi()
	return parsed, metadata.Bin
}

func (s *Suite) readImageRef(name string) string {
	imageRefB, err := ioutil.ReadFile(fmt.Sprintf(".imagerefs/%s", name))
	s.r.NoError(err)
	imageRefB = []byte(strings.TrimSpace(string(imageRefB)))
	s.r.NotEmpty(imageRefB)
	return string(imageRefB)
}

func (s *Suite) ipfsFilesAdd(path string, data interface{}) string {
	b, err := json.Marshal(data)
	s.r.NoError(err)
	s.ipfsClient.FilesRm(s.ctx, path, true)
	err = s.ipfsClient.FilesWrite(s.ctx, path, bytes.NewBuffer(b), ipfsapi.FilesWrite.Create(true))
	s.r.NoError(err)
	stat, err := s.ipfsClient.FilesStat(s.ctx, path)
	s.r.NoError(err)
	return stat.Hash
}

func (s *Suite) ensureAvailability(name string, check func() error) {
	var err error
	for i := 0; i < processStartWaitSeconds*2; i++ {
		time.Sleep(time.Millisecond * 500)
		if err = check(); err == nil {
			return
		}
	}
	s.r.FailNowf("", "failed to ensure '%s' start: %v", name, err)
}

func (s *Suite) TearDownTest() {
	s.fortaProcess = nil
	s.alertServer.Close()
}
func (s *Suite) tearDownProcess(process *os.Process) {
	process.Signal(syscall.SIGINT)
	process.Wait()
}

type Process struct {
	stderr *bytes.Buffer
	stdout *bytes.Buffer
	*os.Process
}

type wrappedBuffer struct {
	w   io.Writer
	buf *bytes.Buffer
}

func (wb *wrappedBuffer) Write(b []byte) (int, error) {
	wb.buf.Write(b)
	return wb.w.Write(b)
}

func (process *Process) HasOutput(s string) bool {
	return strings.Contains(process.stdout.String(), s) || strings.Contains(process.stderr.String(), s)
}

func (s *Suite) forta(fortaDir string, args ...string) {
	dir, err := os.Getwd()
	s.r.NoError(err)

	if fortaDir == "" {
		fortaDir = ".forta"
	}
	fullFortaDir := path.Join(dir, fortaDir)
	coveragePath := path.Join(fortaDir, "coverage", fmt.Sprintf("runner-coverage-%d.tmp", time.Now().Unix()))

	args = append([]string{
		"./forta-test",
		fmt.Sprintf("-test.coverprofile=%s", coveragePath),
	}, args...)
	cmdForta := exec.Command(args[0], args[1:]...)
	cmdForta.Env = append(cmdForta.Env,
		fmt.Sprintf("FORTA_DIR=%s", fullFortaDir),
		"FORTA_PASSPHRASE=0",
	)
	var (
		stderrBuf bytes.Buffer
		stdoutBuf bytes.Buffer
	)
	cmdForta.Stderr = &wrappedBuffer{w: os.Stderr, buf: &stderrBuf}
	cmdForta.Stdout = &wrappedBuffer{w: os.Stdout, buf: &stdoutBuf}

	s.r.NoError(cmdForta.Start())
	s.T().Log("forta cmd started")
	s.fortaProcess = &Process{
		stderr:  &stderrBuf,
		stdout:  &stdoutBuf,
		Process: cmdForta.Process,
	}
}

func (s *Suite) startForta(register ...bool) {
	if register != nil && register[0] {
		s.registerNode()
	}
	s.forta("", "run")
	s.expectUpIn(largeTimeout, allServiceContainers...)
}

func (s *Suite) registerNode() {
	tx, err := s.scannerRegContract.Register(
		s.scanner, ethaccounts.ScannerOwnerAddress, big.NewInt(networkID), "",
	)
	s.r.NoError(err)
	s.ensureTx("ScannerRegistry.register() scan node before 'forta run'", tx)
}

func (s *Suite) stopForta() {
	s.r.NoError(s.fortaProcess.Signal(syscall.SIGINT))
	// s.expectDownIn(largeTimeout, allServiceContainers...)
	_, err := s.fortaProcess.Wait()
	s.r.NoError(err)
}

func (s *Suite) expectIn(timeout time.Duration, conditionFunc func() bool) {
	start := time.Now()
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		if time.Since(start) > timeout {
			s.r.FailNow("expectIn() timed out")
			return
		}
		if ok := conditionFunc(); ok {
			return
		}
	}
}

func (s *Suite) expectUpIn(timeout time.Duration, containerNames ...string) {
	s.expectIn(timeout, func() bool {
		containers, err := s.dockerClient.GetContainers(s.ctx)
		s.r.NoError(err)
		for _, containerName := range containerNames {
			container, ok := containers.ContainsAny(containerName)
			if !ok {
				return false
			}
			if container.State != "running" {
				return false
			}
		}
		return true
	})
}

func (s *Suite) expectDownIn(timeout time.Duration, containerNames ...string) {
	s.expectIn(timeout, func() bool {
		containers, err := s.dockerClient.GetContainers(s.ctx)
		s.r.NoError(err)
		for _, containerName := range containerNames {
			container, ok := containers.FindByName(containerName)
			if !ok {
				continue
			}
			if ok && container.State != "exited" {
				return false
			}
		}
		return true
	})
}

func (s *Suite) sendExploiterTx() {
	gasPrice, err := s.ethClient.SuggestGasPrice(s.ctx)
	s.r.NoError(err)
	nonce, err := s.ethClient.PendingNonceAt(s.ctx, ethaccounts.ExploiterAddress)
	s.r.NoError(err)
	txData := &types.LegacyTx{
		Nonce:    nonce,
		To:       &ethaccounts.ExploiterAddress,
		Value:    big.NewInt(1),
		GasPrice: gasPrice,
		Gas:      100000, // 100k
	}
	tx, err := types.SignNewTx(ethaccounts.ExploiterKey, types.HomesteadSigner{}, txData)
	s.r.NoError(err)

	s.r.NoError(s.ethClient.SendTransaction(s.ctx, tx))
	s.ensureTx("Exploiter account sending 1 Wei to itself", tx)
}
