// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package utils

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strings"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/logging"
	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warpPayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	"github.com/ava-labs/awm-relayer/config"
	offchainregistry "github.com/ava-labs/awm-relayer/messages/off-chain-registry"
	relayercfg "github.com/ava-labs/awm-relayer/relayer/config"
	signatureaggregatorcfg "github.com/ava-labs/awm-relayer/signature-aggregator/config"
	batchcrosschainmessenger "github.com/ava-labs/awm-relayer/tests/abi-bindings/go/BatchCrossChainMessenger"
	relayerUtils "github.com/ava-labs/awm-relayer/utils"
	"github.com/ava-labs/subnet-evm/accounts/abi/bind"
	"github.com/ava-labs/subnet-evm/core/types"
	"github.com/ava-labs/subnet-evm/precompile/contracts/warp"
	predicateutils "github.com/ava-labs/subnet-evm/predicate"
	subnetevmutils "github.com/ava-labs/subnet-evm/utils"
	teleportermessenger "github.com/ava-labs/teleporter/abi-bindings/go/teleporter/TeleporterMessenger"
	"github.com/ava-labs/teleporter/tests/interfaces"
	"github.com/ava-labs/teleporter/tests/utils"
	teleporterTestUtils "github.com/ava-labs/teleporter/tests/utils"
	teleporterUtils "github.com/ava-labs/teleporter/utils/teleporter-utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	. "github.com/onsi/gomega"
)

// Write the test database to /tmp since the data is not needed after the test
var StorageLocation = fmt.Sprintf("%s/.awm-relayer-storage", os.TempDir())

const (
	DefaultRelayerCfgFname             = "relayer-config.json"
	DefaultSignatureAggregatorCfgFname = "signature-aggregator-config.json"
	DBUpdateSeconds                    = 1
)

func BuildAndRunRelayerExecutable(ctx context.Context, relayerConfigPath string) context.CancelFunc {
	// Build the awm-relayer binary
	cmd := exec.Command("./scripts/build_relayer.sh")
	out, err := cmd.CombinedOutput()
	fmt.Println(string(out))
	Expect(err).Should(BeNil())

	// Run awm relayer binary with config path
	relayerCtx, relayerCancel := context.WithCancel(ctx)
	relayerCmd := exec.CommandContext(relayerCtx, "./build/awm-relayer", "--config-file", relayerConfigPath)

	runExecutable(relayerCmd, relayerCtx, "awm-relayer")
	return func() {
		relayerCancel()
		<-relayerCtx.Done()
	}
}

func BuildAndRunSignatureAggregatorExecutable(ctx context.Context, configPath string) context.CancelFunc {
	// Build the signature-aggregator binary
	cmd := exec.Command("./scripts/build_signature_aggregator.sh")
	out, err := cmd.CombinedOutput()
	fmt.Println(string(out))
	Expect(err).Should(BeNil())

	// Run signature-aggregator binary with config path
	log.Info("Instantiating the signature-aggregator executable command")
	log.Info(fmt.Sprintf("./build/signature-aggregator --config-file %s ", configPath))
	aggregatorCtx, aggregatorCancel := context.WithCancel(ctx)
	signatureAggregatorCmd := exec.CommandContext(
		aggregatorCtx,
		"./build/signature-aggregator",
		"--config-file",
		configPath,
	)

	runExecutable(signatureAggregatorCmd, aggregatorCtx, "signature-aggregator")

	return func() {
		aggregatorCancel()
		<-aggregatorCtx.Done()
	}

}

func ReadHexTextFile(filename string) string {
	fileData, err := os.ReadFile(filename)
	Expect(err).Should(BeNil())
	return strings.TrimRight(string(fileData), "\n")
}

// Constructs a relayer config with all subnets as sources and destinations
func CreateDefaultRelayerConfig(
	sourceSubnetsInfo []interfaces.SubnetTestInfo,
	destinationSubnetsInfo []interfaces.SubnetTestInfo,
	teleporterContractAddress common.Address,
	fundedAddress common.Address,
	relayerKey *ecdsa.PrivateKey,
) relayercfg.Config {
	logLevel, err := logging.ToLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		logLevel = logging.Info
	}

	log.Info(
		"Setting up relayer config",
		"logLevel", logLevel.LowerString(),
	)
	// Construct the config values for each subnet
	sources := make([]*relayercfg.SourceBlockchain, len(sourceSubnetsInfo))
	destinations := make([]*relayercfg.DestinationBlockchain, len(destinationSubnetsInfo))
	for i, subnetInfo := range sourceSubnetsInfo {
		host, port, err := teleporterTestUtils.GetURIHostAndPort(subnetInfo.NodeURIs[0])
		Expect(err).Should(BeNil())

		sources[i] = &relayercfg.SourceBlockchain{
			SubnetID:     subnetInfo.SubnetID.String(),
			BlockchainID: subnetInfo.BlockchainID.String(),
			VM:           relayercfg.EVM.String(),
			RPCEndpoint: config.APIConfig{
				BaseURL: fmt.Sprintf("http://%s:%d/ext/bc/%s/rpc", host, port, subnetInfo.BlockchainID.String()),
			},
			WSEndpoint: config.APIConfig{
				BaseURL: fmt.Sprintf("ws://%s:%d/ext/bc/%s/ws", host, port, subnetInfo.BlockchainID.String()),
			},

			MessageContracts: map[string]relayercfg.MessageProtocolConfig{
				teleporterContractAddress.Hex(): {
					MessageFormat: relayercfg.TELEPORTER.String(),
					Settings: map[string]interface{}{
						"reward-address": fundedAddress.Hex(),
					},
				},
				offchainregistry.OffChainRegistrySourceAddress.Hex(): {
					MessageFormat: relayercfg.OFF_CHAIN_REGISTRY.String(),
					Settings: map[string]interface{}{
						"teleporter-registry-address": subnetInfo.TeleporterRegistryAddress.Hex(),
					},
				},
			},
		}

		log.Info(
			"Creating relayer config for source subnet",
			"subnetID", subnetInfo.SubnetID.String(),
			"blockchainID", subnetInfo.BlockchainID.String(),
			"host", host,
			"port", port,
		)
	}

	for i, subnetInfo := range destinationSubnetsInfo {
		host, port, err := teleporterTestUtils.GetURIHostAndPort(subnetInfo.NodeURIs[0])
		Expect(err).Should(BeNil())

		destinations[i] = &relayercfg.DestinationBlockchain{
			SubnetID:     subnetInfo.SubnetID.String(),
			BlockchainID: subnetInfo.BlockchainID.String(),
			VM:           relayercfg.EVM.String(),
			RPCEndpoint: config.APIConfig{
				BaseURL: fmt.Sprintf("http://%s:%d/ext/bc/%s/rpc", host, port, subnetInfo.BlockchainID.String()),
			},
			AccountPrivateKey: relayerUtils.PrivateKeyToString(relayerKey),
		}

		log.Info(
			"Creating relayer config for destination subnet",
			"subnetID", subnetInfo.SubnetID.String(),
			"blockchainID", subnetInfo.BlockchainID.String(),
			"host", host,
			"port", port,
		)
	}

	return relayercfg.Config{
		LogLevel: logging.Info.LowerString(),
		PChainAPI: &config.APIConfig{
			BaseURL: sourceSubnetsInfo[0].NodeURIs[0],
		},
		InfoAPI: &config.APIConfig{
			BaseURL: sourceSubnetsInfo[0].NodeURIs[0],
		},
		StorageLocation:        StorageLocation,
		DBWriteIntervalSeconds: DBUpdateSeconds,
		ProcessMissedBlocks:    false,
		MetricsPort:            9090,
		SourceBlockchains:      sources,
		DestinationBlockchains: destinations,
		APIPort:                8080,
		DeciderURL:             "localhost:50051",
	}
}

func CreateDefaultSignatureAggregatorConfig(
	sourceSubnetsInfo []interfaces.SubnetTestInfo,
) signatureaggregatorcfg.Config {
	logLevel, err := logging.ToLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		logLevel = logging.Info
	}

	log.Info(
		"Setting up signature aggregator config",
		"logLevel", logLevel.LowerString(),
	)
	// Construct the config values for each subnet
	return signatureaggregatorcfg.Config{
		LogLevel: logging.Info.LowerString(),
		PChainAPI: &config.APIConfig{
			BaseURL: sourceSubnetsInfo[0].NodeURIs[0],
		},
		InfoAPI: &config.APIConfig{
			BaseURL: sourceSubnetsInfo[0].NodeURIs[0],
		},
		APIPort:     8080,
		MetricsPort: 8081,
	}
}

func ClearRelayerStorage() error {
	return os.RemoveAll(StorageLocation)
}

func FundRelayers(
	ctx context.Context,
	subnetsInfo []interfaces.SubnetTestInfo,
	fundedKey *ecdsa.PrivateKey,
	relayerKey *ecdsa.PrivateKey,
) {
	relayerAddress := crypto.PubkeyToAddress(relayerKey.PublicKey)
	fundAmount := big.NewInt(0).Mul(big.NewInt(1e18), big.NewInt(10)) // 10eth

	for _, subnetInfo := range subnetsInfo {
		fundRelayerTx := utils.CreateNativeTransferTransaction(
			ctx, subnetInfo, fundedKey, relayerAddress, fundAmount,
		)
		utils.SendTransactionAndWaitForSuccess(ctx, subnetInfo, fundRelayerTx)
	}
}

func SendBasicTeleporterMessageAsync(
	ctx context.Context,
	source interfaces.SubnetTestInfo,
	destination interfaces.SubnetTestInfo,
	fundedKey *ecdsa.PrivateKey,
	destinationAddress common.Address,
	ids chan<- ids.ID,
) {
	input := teleportermessenger.TeleporterMessageInput{
		DestinationBlockchainID: destination.BlockchainID,
		DestinationAddress:      destinationAddress,
		FeeInfo: teleportermessenger.TeleporterFeeInfo{
			FeeTokenAddress: common.Address{},
			Amount:          big.NewInt(0),
		},
		RequiredGasLimit:        big.NewInt(1),
		AllowedRelayerAddresses: []common.Address{},
		Message:                 []byte{1, 2, 3, 4},
	}

	// Send a transaction to the Teleporter contract
	log.Info(
		"Sending teleporter transaction",
		"sourceBlockchainID", source.BlockchainID,
		"destinationBlockchainID", destination.BlockchainID,
	)
	_, teleporterMessageID := teleporterTestUtils.SendCrossChainMessageAndWaitForAcceptance(
		ctx,
		source,
		destination,
		input,
		fundedKey,
	)
	ids <- teleporterMessageID
}

func SendBasicTeleporterMessage(
	ctx context.Context,
	source interfaces.SubnetTestInfo,
	destination interfaces.SubnetTestInfo,
	fundedKey *ecdsa.PrivateKey,
	destinationAddress common.Address,
) (*types.Receipt, teleportermessenger.TeleporterMessage, ids.ID) {
	input := teleportermessenger.TeleporterMessageInput{
		DestinationBlockchainID: destination.BlockchainID,
		DestinationAddress:      destinationAddress,
		FeeInfo: teleportermessenger.TeleporterFeeInfo{
			FeeTokenAddress: common.Address{},
			Amount:          big.NewInt(0),
		},
		RequiredGasLimit:        big.NewInt(1),
		AllowedRelayerAddresses: []common.Address{},
		Message:                 []byte{1, 2, 3, 4},
	}

	// Send a transaction to the Teleporter contract
	log.Info(
		"Sending teleporter transaction",
		"sourceBlockchainID", source.BlockchainID,
		"destinationBlockchainID", destination.BlockchainID,
	)
	receipt, teleporterMessageID := teleporterTestUtils.SendCrossChainMessageAndWaitForAcceptance(
		ctx,
		source,
		destination,
		input,
		fundedKey,
	)
	sendEvent, err := teleporterTestUtils.GetEventFromLogs(
		receipt.Logs,
		source.TeleporterMessenger.ParseSendCrossChainMessage,
	)
	Expect(err).Should(BeNil())

	return receipt, sendEvent.Message, teleporterMessageID
}

func RelayBasicMessage(
	ctx context.Context,
	source interfaces.SubnetTestInfo,
	destination interfaces.SubnetTestInfo,
	teleporterContractAddress common.Address,
	fundedKey *ecdsa.PrivateKey,
	destinationAddress common.Address,
) {
	newHeadsDest := make(chan *types.Header, 10)
	sub, err := destination.WSClient.SubscribeNewHead(ctx, newHeadsDest)
	Expect(err).Should(BeNil())
	defer sub.Unsubscribe()

	_, teleporterMessage, teleporterMessageID := SendBasicTeleporterMessage(
		ctx,
		source,
		destination,
		fundedKey,
		destinationAddress,
	)

	log.Info("Waiting for new block confirmation")
	newHead := <-newHeadsDest
	blockNumber := newHead.Number
	log.Info(
		"Received new head",
		"height", blockNumber.Uint64(),
		"hash", newHead.Hash(),
	)
	block, err := destination.RPCClient.BlockByNumber(ctx, blockNumber)
	Expect(err).Should(BeNil())
	log.Info(
		"Got block",
		"blockHash", block.Hash(),
		"blockNumber", block.NumberU64(),
		"transactions", block.Transactions(),
		"numTransactions", len(block.Transactions()),
		"block", block,
	)
	accessLists := block.Transactions()[0].AccessList()
	Expect(len(accessLists)).Should(Equal(1))
	Expect(accessLists[0].Address).Should(Equal(warp.Module.Address))

	// Check the transaction storage key has warp message we're expecting
	storageKeyHashes := accessLists[0].StorageKeys
	packedPredicate := subnetevmutils.HashSliceToBytes(storageKeyHashes)
	predicateBytes, err := predicateutils.UnpackPredicate(packedPredicate)
	Expect(err).Should(BeNil())
	receivedWarpMessage, err := avalancheWarp.ParseMessage(predicateBytes)
	Expect(err).Should(BeNil())

	// Check that the transaction has successful receipt status
	txHash := block.Transactions()[0].Hash()
	receipt, err := destination.RPCClient.TransactionReceipt(ctx, txHash)
	Expect(err).Should(BeNil())
	Expect(receipt.Status).Should(Equal(types.ReceiptStatusSuccessful))

	// Check that the transaction emits ReceiveCrossChainMessage
	receiveEvent, err := teleporterTestUtils.GetEventFromLogs(
		receipt.Logs,
		destination.TeleporterMessenger.ParseReceiveCrossChainMessage,
	)
	Expect(err).Should(BeNil())
	Expect(receiveEvent.SourceBlockchainID[:]).Should(Equal(source.BlockchainID[:]))
	Expect(receiveEvent.MessageID[:]).Should(Equal(teleporterMessageID[:]))

	//
	// Validate Received Warp Message Values
	//
	log.Info("Validating received warp message")
	Expect(receivedWarpMessage.SourceChainID).Should(Equal(source.BlockchainID))
	addressedPayload, err := warpPayload.ParseAddressedCall(receivedWarpMessage.Payload)
	Expect(err).Should(BeNil())

	// Check that the teleporter message is correct
	// We don't validate the entire message, since the message receipts
	// are populated by the Teleporter contract
	receivedTeleporterMessage, err := teleportermessenger.UnpackTeleporterMessage(addressedPayload.Payload)
	Expect(err).Should(BeNil())

	receivedMessageID, err := teleporterUtils.CalculateMessageID(
		teleporterContractAddress,
		source.BlockchainID,
		destination.BlockchainID,
		teleporterMessage.MessageNonce,
	)
	Expect(err).Should(BeNil())
	Expect(receivedMessageID).Should(Equal(teleporterMessageID))
	Expect(receivedTeleporterMessage.OriginSenderAddress).Should(Equal(teleporterMessage.OriginSenderAddress))
	receivedDestinationID, err := ids.ToID(receivedTeleporterMessage.DestinationBlockchainID[:])
	Expect(err).Should(BeNil())
	Expect(receivedDestinationID).Should(Equal(destination.BlockchainID))
	Expect(receivedTeleporterMessage.DestinationAddress).Should(Equal(teleporterMessage.DestinationAddress))
	Expect(receivedTeleporterMessage.RequiredGasLimit.Uint64()).Should(Equal(teleporterMessage.RequiredGasLimit.Uint64()))
	Expect(receivedTeleporterMessage.Message).Should(Equal(teleporterMessage.Message))
}

func WriteRelayerConfig(relayerConfig relayercfg.Config, fname string) string {
	data, err := json.MarshalIndent(relayerConfig, "", "\t")
	Expect(err).Should(BeNil())

	f, err := os.CreateTemp(os.TempDir(), fname)
	Expect(err).Should(BeNil())

	_, err = f.Write(data)
	Expect(err).Should(BeNil())
	relayerConfigPath := f.Name()

	log.Info("Created awm-relayer config", "configPath", relayerConfigPath, "config", string(data))
	return relayerConfigPath
}

// TODO define interface over Config and write a generic function to write either config
func WriteSignatureAggregatorConfig(signatureAggregatorConfig signatureaggregatorcfg.Config, fname string) string {
	data, err := json.MarshalIndent(signatureAggregatorConfig, "", "\t")
	Expect(err).Should(BeNil())

	f, err := os.CreateTemp(os.TempDir(), fname)
	Expect(err).Should(BeNil())

	_, err = f.Write(data)
	Expect(err).Should(BeNil())
	signatureAggregatorConfigPath := f.Name()

	log.Info("Created signature-aggregator config", "configPath", signatureAggregatorConfigPath, "config", string(data))
	return signatureAggregatorConfigPath
}

func TriggerProcessMissedBlocks(
	ctx context.Context,
	sourceSubnetInfo interfaces.SubnetTestInfo,
	destinationSubnetInfo interfaces.SubnetTestInfo,
	currRelayerCleanup context.CancelFunc,
	currrentRelayerConfig relayercfg.Config,
	fundedAddress common.Address,
	fundedKey *ecdsa.PrivateKey,
) {
	// First, make sure the relayer is stopped
	currRelayerCleanup()

	// Subscribe to the destination chain
	newHeads := make(chan *types.Header, 10)
	sub, err := destinationSubnetInfo.WSClient.SubscribeNewHead(ctx, newHeads)
	Expect(err).Should(BeNil())
	defer sub.Unsubscribe()

	// Send three Teleporter messages from subnet A to subnet B
	log.Info("Sending three Teleporter messages from subnet A to subnet B")
	_, _, id1 := SendBasicTeleporterMessage(ctx, sourceSubnetInfo, destinationSubnetInfo, fundedKey, fundedAddress)
	_, _, id2 := SendBasicTeleporterMessage(ctx, sourceSubnetInfo, destinationSubnetInfo, fundedKey, fundedAddress)
	_, _, id3 := SendBasicTeleporterMessage(ctx, sourceSubnetInfo, destinationSubnetInfo, fundedKey, fundedAddress)

	currHeight, err := sourceSubnetInfo.RPCClient.BlockNumber(ctx)
	Expect(err).Should(BeNil())
	log.Info("Current block height", "height", currHeight)

	// Configure the relayer such that it will only process the last of the three messages sent above.
	// The relayer DB stores the height of the block *before* the first message, so by setting the
	// ProcessHistoricalBlocksFromHeight to the block height of the *third* message, we expect the relayer to skip
	// the first two messages on startup, but process the third.
	modifiedRelayerConfig := currrentRelayerConfig
	modifiedRelayerConfig.SourceBlockchains[0].ProcessHistoricalBlocksFromHeight = currHeight
	modifiedRelayerConfig.ProcessMissedBlocks = true
	relayerConfigPath := WriteRelayerConfig(modifiedRelayerConfig, DefaultRelayerCfgFname)

	log.Info("Starting the relayer")
	relayerCleanup := BuildAndRunRelayerExecutable(ctx, relayerConfigPath)
	defer relayerCleanup()
	log.Info("Waiting for a new block confirmation on the destination")
	<-newHeads
	delivered1, err := destinationSubnetInfo.TeleporterMessenger.MessageReceived(
		&bind.CallOpts{}, id1,
	)
	Expect(err).Should(BeNil())
	delivered2, err := destinationSubnetInfo.TeleporterMessenger.MessageReceived(
		&bind.CallOpts{}, id2,
	)
	Expect(err).Should(BeNil())
	delivered3, err := destinationSubnetInfo.TeleporterMessenger.MessageReceived(
		&bind.CallOpts{}, id3,
	)
	Expect(err).Should(BeNil())
	Expect(delivered1).Should(BeFalse())
	Expect(delivered2).Should(BeFalse())
	Expect(delivered3).Should(BeTrue())
}

func DeployBatchCrossChainMessenger(
	ctx context.Context,
	senderKey *ecdsa.PrivateKey,
	teleporterManager common.Address,
	subnet interfaces.SubnetTestInfo,
) (common.Address, *batchcrosschainmessenger.BatchCrossChainMessenger) {
	opts, err := bind.NewKeyedTransactorWithChainID(
		senderKey, subnet.EVMChainID)
	Expect(err).Should(BeNil())
	address, tx, exampleMessenger, err := batchcrosschainmessenger.DeployBatchCrossChainMessenger(
		opts,
		subnet.RPCClient,
		subnet.TeleporterRegistryAddress,
		teleporterManager,
	)
	Expect(err).Should(BeNil())

	// Wait for the transaction to be mined
	utils.WaitForTransactionSuccess(ctx, subnet, tx.Hash())

	return address, exampleMessenger
}

func runExecutable(
	cmd *exec.Cmd,
	ctx context.Context,
	appName string,
) {
	cmdOutput := make(chan string)

	// Set up a pipe to capture the command's output
	cmdStdOutReader, err := cmd.StdoutPipe()
	Expect(err).Should(BeNil())
	cmdStdErrReader, err := cmd.StderrPipe()
	Expect(err).Should(BeNil())

	// Start the command
	log.Info("Starting executable", "appName", appName)
	err = cmd.Start()
	Expect(err).Should(BeNil())

	// Start goroutines to read and output the command's stdout and stderr
	go func() {
		scanner := bufio.NewScanner(cmdStdOutReader)
		for scanner.Scan() {
			log.Info(scanner.Text())
		}
		cmdOutput <- "Command execution finished"
	}()
	go func() {
		scanner := bufio.NewScanner(cmdStdErrReader)
		for scanner.Scan() {
			log.Error(scanner.Text())
		}
		cmdOutput <- "Command execution finished"
	}()
	// Spawn a goroutine that will panic if the aggregator exits abnormally.
	go func() {
		err := cmd.Wait()
		// Context cancellation is the only expected way for the process to exit, otherwise panic
		if !errors.Is(ctx.Err(), context.Canceled) {
			panic(fmt.Errorf("%s exited abnormally: %w", appName, err))
		}
	}()
}
