package framework

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xPolygon/polygon-edge/command/genesis"
	"github.com/0xPolygon/polygon-edge/command/rootchain/helper"
	"github.com/0xPolygon/polygon-edge/helper/common"
	"github.com/0xPolygon/polygon-edge/txrelayer"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/stretchr/testify/require"
	"github.com/umbracle/ethgo"
	"github.com/umbracle/ethgo/abi"
	"github.com/umbracle/ethgo/jsonrpc"
	"github.com/umbracle/ethgo/wallet"
)

const (
	// envE2ETestsEnabled signal whether the e2e tests will run
	envE2ETestsEnabled = "E2E_TESTS"

	// envLogsEnabled signal whether the output of the nodes get piped to a log file
	envLogsEnabled = "E2E_LOGS"

	// envLogLevel specifies log level of each node
	envLogLevel = "E2E_LOG_LEVEL"

	// envStdoutEnabled signal whether the output of the nodes get piped to stdout
	envStdoutEnabled = "E2E_STDOUT"

	// envE2ETestsType used just to display type of test if skipped
	envE2ETestsType = "E2E_TESTS_TYPE"
)

const (
	// prefix for validator directory
	defaultValidatorPrefix = "test-chain-"
)

var startTime int64

func init() {
	startTime = time.Now().UTC().UnixMilli()
}

func resolveBinary() string {
	bin := os.Getenv("EDGE_BINARY")
	if bin != "" {
		return bin
	}
	// fallback
	return "polygon-edge"
}

type TestClusterConfig struct {
	t *testing.T

	Name              string
	Premine           []string // address[:amount]
	PremineValidators []string // address:[amount]
	HasBridge         bool
	BootnodeCount     int
	NonValidatorCount int
	WithLogs          bool
	WithStdout        bool
	LogsDir           string
	TmpDir            string
	BlockGasLimit     uint64
	ValidatorPrefix   string
	Binary            string
	ValidatorSetSize  uint64
	EpochSize         int
	EpochReward       int
	SecretsCallback   func([]types.Address, *TestClusterConfig)

	ContractDeployerAllowListAdmin   []types.Address
	ContractDeployerAllowListEnabled []types.Address

	NumBlockConfirmations uint64

	InitialTrieDB    string
	InitialStateRoot types.Hash

	logsDirOnce sync.Once
}

func (c *TestClusterConfig) Dir(name string) string {
	return filepath.Join(c.TmpDir, name)
}

func (c *TestClusterConfig) GetStdout(name string, custom ...io.Writer) io.Writer {
	writers := []io.Writer{}

	if c.WithLogs {
		c.logsDirOnce.Do(func() {
			c.initLogsDir()
		})

		f, err := os.OpenFile(filepath.Join(c.LogsDir, name+".log"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0600)
		if err != nil {
			c.t.Fatal(err)
		}

		writers = append(writers, f)

		c.t.Cleanup(func() {
			err = f.Close()
			if err != nil {
				c.t.Logf("Failed to close file. Error: %s", err)
			}
		})
	}

	if c.WithStdout {
		writers = append(writers, os.Stdout)
	}

	if len(custom) > 0 {
		writers = append(writers, custom...)
	}

	if len(writers) == 0 {
		return io.Discard
	}

	return io.MultiWriter(writers...)
}

func (c *TestClusterConfig) initLogsDir() {
	logsDir := path.Join("../..", fmt.Sprintf("e2e-logs-%d", startTime), c.t.Name())

	if err := common.CreateDirSafe(logsDir, 0750); err != nil {
		c.t.Fatal(err)
	}

	c.t.Logf("logs enabled for e2e test: %s", logsDir)
	c.LogsDir = logsDir
}

type TestCluster struct {
	Config      *TestClusterConfig
	Servers     []*TestServer
	Bridge      *TestBridge
	initialPort int64

	once         sync.Once
	failCh       chan struct{}
	executionErr error

	sendTxnLock sync.Mutex
}

type ClusterOption func(*TestClusterConfig)

func WithPremine(addresses ...types.Address) ClusterOption {
	return func(h *TestClusterConfig) {
		for _, a := range addresses {
			h.Premine = append(h.Premine, a.String())
		}
	}
}

func WithSecretsCallback(fn func([]types.Address, *TestClusterConfig)) ClusterOption {
	return func(h *TestClusterConfig) {
		h.SecretsCallback = fn
	}
}

func WithBridge() ClusterOption {
	return func(h *TestClusterConfig) {
		h.HasBridge = true
	}
}

func WithNonValidators(num int) ClusterOption {
	return func(h *TestClusterConfig) {
		h.NonValidatorCount = num
	}
}

func WithValidatorSnapshot(validatorsLen uint64) ClusterOption {
	return func(h *TestClusterConfig) {
		h.ValidatorSetSize = validatorsLen
	}
}

func WithGenesisState(databasePath string, stateRoot types.Hash) ClusterOption {
	return func(h *TestClusterConfig) {
		h.InitialTrieDB = databasePath
		h.InitialStateRoot = stateRoot
	}
}

func WithBootnodeCount(cnt int) ClusterOption {
	return func(h *TestClusterConfig) {
		h.BootnodeCount = cnt
	}
}

func WithEpochSize(epochSize int) ClusterOption {
	return func(h *TestClusterConfig) {
		h.EpochSize = epochSize
	}
}

func WithEpochReward(epochReward int) ClusterOption {
	return func(h *TestClusterConfig) {
		h.EpochReward = epochReward
	}
}

func WithBlockGasLimit(blockGasLimit uint64) ClusterOption {
	return func(h *TestClusterConfig) {
		h.BlockGasLimit = blockGasLimit
	}
}

func WithNumBlockConfirmations(numBlockConfirmations uint64) ClusterOption {
	return func(h *TestClusterConfig) {
		h.NumBlockConfirmations = numBlockConfirmations
	}
}

func WithContractDeployerAllowListAdmin(addr types.Address) ClusterOption {
	return func(h *TestClusterConfig) {
		h.ContractDeployerAllowListAdmin = append(h.ContractDeployerAllowListAdmin, addr)
	}
}

func WithContractDeployerAllowListEnabled(addr types.Address) ClusterOption {
	return func(h *TestClusterConfig) {
		h.ContractDeployerAllowListEnabled = append(h.ContractDeployerAllowListEnabled, addr)
	}
}

func isTrueEnv(e string) bool {
	return strings.ToLower(os.Getenv(e)) == "true"
}

func NewTestCluster(t *testing.T, validatorsCount int, opts ...ClusterOption) *TestCluster {
	t.Helper()

	var err error

	config := &TestClusterConfig{
		t:                 t,
		WithLogs:          isTrueEnv(envLogsEnabled),
		WithStdout:        isTrueEnv(envStdoutEnabled),
		Binary:            resolveBinary(),
		EpochSize:         10,
		EpochReward:       1,
		BlockGasLimit:     1e7, // 10M
		PremineValidators: []string{},
	}

	if config.ValidatorPrefix == "" {
		config.ValidatorPrefix = defaultValidatorPrefix
	}

	for _, opt := range opts {
		opt(config)
	}

	if !isTrueEnv(envE2ETestsEnabled) {
		testType := os.Getenv(envE2ETestsType)
		if testType == "" {
			testType = "integration"
		}

		t.Skip(fmt.Sprintf("%s tests are disabled.", testType))
	}

	config.TmpDir, err = os.MkdirTemp("/tmp", "e2e-polybft-")
	require.NoError(t, err)

	cluster := &TestCluster{
		Servers:     []*TestServer{},
		Config:      config,
		initialPort: 30300,
		failCh:      make(chan struct{}),
		once:        sync.Once{},
	}

	{
		// run init accounts
		addresses, err := cluster.InitSecrets(cluster.Config.ValidatorPrefix, validatorsCount)
		require.NoError(t, err)

		if cluster.Config.SecretsCallback != nil {
			cluster.Config.SecretsCallback(addresses, cluster.Config)
		}
	}

	manifestPath := path.Join(config.TmpDir, "manifest.json")
	args := []string{
		"manifest",
		"--path", manifestPath,
		"--validators-path", config.TmpDir,
		"--validators-prefix", cluster.Config.ValidatorPrefix,
	}

	// premine validators
	for _, premineValidator := range cluster.Config.PremineValidators {
		args = append(args, "--premine-validators", premineValidator)
	}

	// run manifest file creation
	require.NoError(t, cluster.cmdRun(args...))

	if cluster.Config.HasBridge {
		// start bridge
		cluster.Bridge, err = NewTestBridge(t, cluster.Config)
		require.NoError(t, err)
	}

	// in case no validators are specified in opts, all nodes will be validators
	if cluster.Config.ValidatorSetSize == 0 {
		cluster.Config.ValidatorSetSize = uint64(validatorsCount)
	}

	if cluster.Config.HasBridge {
		err := cluster.Bridge.deployRootchainContracts(manifestPath)
		require.NoError(t, err)

		err = cluster.Bridge.fundRootchainValidators()
		require.NoError(t, err)
	}

	{
		// run genesis configuration population
		args := []string{
			"genesis",
			"--manifest", manifestPath,
			"--consensus", "polybft",
			"--dir", path.Join(config.TmpDir, "genesis.json"),
			"--block-gas-limit", strconv.FormatUint(cluster.Config.BlockGasLimit, 10),
			"--epoch-size", strconv.Itoa(cluster.Config.EpochSize),
			"--epoch-reward", strconv.Itoa(cluster.Config.EpochReward),
			"--premine", "0x0000000000000000000000000000000000000000",
			"--trieroot", cluster.Config.InitialStateRoot.String(),
		}

		if len(cluster.Config.Premine) != 0 {
			for _, premine := range cluster.Config.Premine {
				args = append(args, "--premine", premine)
			}
		}

		if cluster.Config.HasBridge {
			rootchainIP, err := helper.ReadRootchainIP()
			require.NoError(t, err)
			args = append(args, "--bridge-json-rpc", rootchainIP)
		}

		validators, err := genesis.ReadValidatorsByPrefix(
			cluster.Config.TmpDir, cluster.Config.ValidatorPrefix)
		require.NoError(t, err)

		if cluster.Config.BootnodeCount > 0 {
			bootNodesCnt := cluster.Config.BootnodeCount
			if len(validators) < bootNodesCnt {
				bootNodesCnt = len(validators)
			}

			for i := 0; i < bootNodesCnt; i++ {
				args = append(args, "--bootnode", validators[i].MultiAddr)
			}
		}

		if cluster.Config.ValidatorSetSize > 0 {
			args = append(args, "--validator-set-size", fmt.Sprint(cluster.Config.ValidatorSetSize))
		}

		if len(cluster.Config.ContractDeployerAllowListAdmin) != 0 {
			args = append(args, "--contract-deployer-allow-list-admin",
				strings.Join(sliceAddressToSliceString(cluster.Config.ContractDeployerAllowListAdmin), ","))
		}

		if len(cluster.Config.ContractDeployerAllowListEnabled) != 0 {
			args = append(args, "--contract-deployer-allow-list-enabled",
				strings.Join(sliceAddressToSliceString(cluster.Config.ContractDeployerAllowListEnabled), ","))
		}

		// run cmd init-genesis with all the arguments
		err = cluster.cmdRun(args...)
		require.NoError(t, err)
	}

	for i := 1; i <= int(cluster.Config.ValidatorSetSize); i++ {
		cluster.InitTestServer(t, i, true, cluster.Config.HasBridge && i == 1 /* relayer */)
	}

	for i := 1; i <= cluster.Config.NonValidatorCount; i++ {
		offsetIndex := i + int(cluster.Config.ValidatorSetSize)
		cluster.InitTestServer(t, offsetIndex, false, false /* relayer */)
	}

	return cluster
}

func (c *TestCluster) InitTestServer(t *testing.T, i int, isValidator bool, relayer bool) {
	t.Helper()

	logLevel := os.Getenv(envLogLevel)

	dataDir := c.Config.Dir(c.Config.ValidatorPrefix + strconv.Itoa(i))
	if c.Config.InitialTrieDB != "" {
		err := CopyDir(c.Config.InitialTrieDB, filepath.Join(dataDir, "trie"))
		if err != nil {
			t.Fatal(err)
		}
	}

	srv := NewTestServer(t, c.Config, func(config *TestServerConfig) {
		config.DataDir = dataDir
		config.Seal = isValidator
		config.Chain = c.Config.Dir("genesis.json")
		config.P2PPort = c.getOpenPort()
		config.LogLevel = logLevel
		config.Relayer = relayer
		config.NumBlockConfirmations = c.Config.NumBlockConfirmations
	})

	// watch the server for stop signals. It is important to fix the specific
	// 'node' reference since 'TestServer' creates a new one if restarted.
	go func(node *node) {
		<-node.Wait()

		if !node.ExitResult().Signaled {
			c.Fail(fmt.Errorf("server at dir '%s' has stopped unexpectedly", dataDir))
		}
	}(srv.node)

	c.Servers = append(c.Servers, srv)
}

func (c *TestCluster) cmdRun(args ...string) error {
	return runCommand(c.Config.Binary, args, c.Config.GetStdout(args[0]))
}

func (c *TestCluster) Fail(err error) {
	c.once.Do(func() {
		c.executionErr = err
		close(c.failCh)
	})
}

func (c *TestCluster) Stop() {
	if c.Bridge != nil {
		c.Bridge.Stop()
	}

	for _, srv := range c.Servers {
		if srv.isRunning() {
			srv.Stop()
		}
	}
}

func (c *TestCluster) Stats(t *testing.T) {
	t.Helper()

	for index, i := range c.Servers {
		if !i.isRunning() {
			continue
		}

		num, err := i.JSONRPC().Eth().BlockNumber()
		t.Log("Stats node", index, "err", err, "block", num, "validator", i.config.Seal)
	}
}

func (c *TestCluster) WaitUntil(timeout, pollFrequency time.Duration, handler func() bool) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return fmt.Errorf("timeout")
		case <-c.failCh:
			return c.executionErr
		case <-time.After(pollFrequency):
		}

		if handler() {
			return nil
		}
	}
}

func (c *TestCluster) WaitForReady(t *testing.T) {
	t.Helper()

	require.NoError(t, c.WaitForBlock(3, 1*time.Minute))
}

func (c *TestCluster) WaitForBlock(n uint64, timeout time.Duration) error {
	timer := time.NewTimer(timeout)

	ok := false
	for !ok {
		select {
		case <-timer.C:
			return fmt.Errorf("wait for block timeout")
		case <-time.After(2 * time.Second):
		}

		ok = true

		for _, i := range c.Servers {
			if !i.isRunning() {
				continue
			}

			num, err := i.JSONRPC().Eth().BlockNumber()

			if err != nil || num < n {
				ok = false

				break
			}
		}
	}

	return nil
}

// WaitForGeneric waits until all running servers returns true from fn callback or timeout defined by dur occurs
func (c *TestCluster) WaitForGeneric(dur time.Duration, fn func(*TestServer) bool) error {
	return c.WaitUntil(dur, 2*time.Second, func() bool {
		for _, srv := range c.Servers {
			// query only running servers
			if srv.isRunning() && !fn(srv) {
				return false
			}
		}

		return true
	})
}

func (c *TestCluster) getOpenPort() int64 {
	c.initialPort++

	return c.initialPort
}

// runCommand executes command with given arguments
func runCommand(binary string, args []string, stdout io.Writer) error {
	var stdErr bytes.Buffer

	cmd := exec.Command(binary, args...)
	cmd.Stderr = &stdErr
	cmd.Stdout = stdout

	if err := cmd.Run(); err != nil {
		if stdErr.Len() > 0 {
			return fmt.Errorf("failed to execute command: %s", stdErr.String())
		}

		return fmt.Errorf("failed to execute command: %w", err)
	}

	if stdErr.Len() > 0 {
		return fmt.Errorf("error during command execution: %s", stdErr.String())
	}

	return nil
}

// RunEdgeCommand - calls a command line edge function
func RunEdgeCommand(args []string, stdout io.Writer) error {
	return runCommand(resolveBinary(), args, stdout)
}

// InitSecrets initializes account(s) secrets with given prefix.
// (secrets are being stored in the temp directory created by given e2e test execution)
func (c *TestCluster) InitSecrets(prefix string, count int) ([]types.Address, error) {
	var b bytes.Buffer

	args := []string{
		"polybft-secrets",
		"--data-dir", path.Join(c.Config.TmpDir, prefix),
		"--num", strconv.Itoa(count),
		"--insecure",
	}
	stdOut := c.Config.GetStdout("polybft-secrets", &b)

	if err := runCommand(c.Config.Binary, args, stdOut); err != nil {
		return nil, err
	}

	re := regexp.MustCompile("\\(address\\) = 0x([a-fA-F0-9]+)")
	parsed := re.FindAllStringSubmatch(b.String(), -1)
	result := make([]types.Address, len(parsed))

	for i, v := range parsed {
		result[i] = types.StringToAddress(v[1])
	}

	return result, nil
}

func (c *TestCluster) ExistsCode(t *testing.T, addr ethgo.Address) bool {
	t.Helper()

	client, err := jsonrpc.NewClient(c.Servers[0].JSONRPCAddr())
	require.NoError(t, err)

	code, err := client.Eth().GetCode(addr, ethgo.Latest)
	if err != nil {
		return false
	}

	return code != "0x"
}

func (c *TestCluster) Call(t *testing.T, to types.Address, method *abi.Method,
	args ...interface{}) map[string]interface{} {
	t.Helper()

	client, err := jsonrpc.NewClient(c.Servers[0].JSONRPCAddr())
	require.NoError(t, err)

	input, err := method.Encode(args)
	require.NoError(t, err)

	toAddr := ethgo.Address(to)

	msg := &ethgo.CallMsg{
		To:   &toAddr,
		Data: input,
	}
	resp, err := client.Eth().Call(msg, ethgo.Latest)
	require.NoError(t, err)

	data, err := hex.DecodeString(resp[2:])
	require.NoError(t, err)

	output, err := method.Decode(data)
	require.NoError(t, err)

	return output
}

func (c *TestCluster) Deploy(t *testing.T, sender ethgo.Key, bytecode []byte) *TestTxn {
	t.Helper()

	return c.SendTxn(t, sender, &ethgo.Transaction{Input: bytecode})
}

func (c *TestCluster) Transfer(t *testing.T, sender ethgo.Key, target types.Address, value *big.Int) *TestTxn {
	t.Helper()

	targetAddr := ethgo.Address(target)

	return c.SendTxn(t, sender, &ethgo.Transaction{To: &targetAddr, Value: value})
}

func (c *TestCluster) MethodTxn(t *testing.T, sender ethgo.Key, target types.Address, input []byte) *TestTxn {
	t.Helper()

	targetAddr := ethgo.Address(target)

	return c.SendTxn(t, sender, &ethgo.Transaction{To: &targetAddr, Input: input})
}

// SendTxn sends a transaction
func (c *TestCluster) SendTxn(t *testing.T, sender ethgo.Key, txn *ethgo.Transaction) *TestTxn {
	t.Helper()

	// since we might use get nonce to query the latest nonce and that value is only
	// updated if the transaction is on the pool, it is recommended to lock the whole
	// execution in case we send multiple transactions from the same account and we expect
	// to get a sequential nonce order.
	c.sendTxnLock.Lock()
	defer c.sendTxnLock.Unlock()

	client, err := jsonrpc.NewClient(c.Servers[0].JSONRPCAddr())
	require.NoError(t, err)

	// initialize transaction values if not set
	if txn.Nonce == 0 {
		nonce, err := client.Eth().GetNonce(sender.Address(), ethgo.Latest)
		require.NoError(t, err)

		txn.Nonce = nonce
	}

	if txn.GasPrice == 0 {
		txn.GasPrice = txrelayer.DefaultGasPrice
	}

	if txn.Gas == 0 {
		txn.Gas = txrelayer.DefaultGasLimit
	}

	chainID, err := client.Eth().ChainID()
	require.NoError(t, err)

	signer := wallet.NewEIP155Signer(chainID.Uint64())
	signedTxn, err := signer.SignTx(txn, sender)
	require.NoError(t, err)

	txnRaw, err := signedTxn.MarshalRLPTo(nil)
	require.NoError(t, err)

	hash, err := client.Eth().SendRawTransaction(txnRaw)
	require.NoError(t, err)

	tTxn := &TestTxn{
		client: client.Eth(),
		txn:    txn,
		hash:   hash,
	}

	return tTxn
}

type TestTxn struct {
	client  *jsonrpc.Eth
	hash    ethgo.Hash
	txn     *ethgo.Transaction
	receipt *ethgo.Receipt
}

// Txn returns the raw transaction that was sent
func (t *TestTxn) Txn() *ethgo.Transaction {
	return t.txn
}

// Receipt returns the receipt of the transaction
func (t *TestTxn) Receipt() *ethgo.Receipt {
	return t.receipt
}

// Succeed returns whether the transaction succeed and it was not reverted
func (t *TestTxn) Succeed() bool {
	return t.receipt.Status == uint64(types.ReceiptSuccess)
}

// Failed returns whether the transaction failed
func (t *TestTxn) Failed() bool {
	return t.receipt.Status == uint64(types.ReceiptFailed)
}

// Reverted returns whether the transaction failed and was reverted consuming
// all the gas from the call
func (t *TestTxn) Reverted() bool {
	return t.receipt.Status == uint64(types.ReceiptFailed) && t.txn.Gas == t.receipt.GasUsed
}

// Wait waits for the transaction to be executed
func (t *TestTxn) Wait() error {
	tt := time.NewTimer(1 * time.Minute)

	for {
		select {
		case <-time.After(100 * time.Millisecond):
			receipt, err := t.client.GetTransactionReceipt(t.hash)
			if err != nil {
				if err.Error() != "not found" {
					return err
				}
			}

			if receipt != nil {
				t.receipt = receipt

				return nil
			}

		case <-tt.C:
			return fmt.Errorf("timeout")
		}
	}
}

func sliceAddressToSliceString(addrs []types.Address) []string {
	res := make([]string, len(addrs))
	for indx, addr := range addrs {
		res[indx] = addr.String()
	}

	return res
}

func CopyDir(source, destination string) error {
	err := os.Mkdir(destination, 0755)
	if err != nil {
		return err
	}

	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		relPath := strings.Replace(path, source, "", 1)
		if relPath == "" {
			return nil
		}

		data, err := ioutil.ReadFile(filepath.Join(source, relPath))
		if err != nil {
			return err
		}

		return ioutil.WriteFile(filepath.Join(destination, relPath), data, 0600)
	})
}
