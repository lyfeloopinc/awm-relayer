// Copyright (C) 2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package peers

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/message"
	"github.com/ava-labs/avalanchego/network"
	avagoCommon "github.com/ava-labs/avalanchego/snow/engine/common"
	snowVdrs "github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/subnets"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/set"
	libPeers "github.com/ava-labs/awm-relayer/lib/peers"
	"github.com/ava-labs/awm-relayer/peers/validators"
	"github.com/ava-labs/awm-relayer/signature-aggregator/config"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	InboundMessageChannelSize = 1000
	DefaultAppRequestTimeout  = time.Second * 2
)

type AppRequestNetwork struct {
	network         network.Network
	handler         *libPeers.RelayerExternalHandler
	infoAPI         *libPeers.InfoAPI
	logger          logging.Logger
	lock            *sync.Mutex
	validatorClient *validators.CanonicalValidatorClient
}

// NewNetwork creates a p2p network client for interacting with validators
func NewNetwork(
	logLevel logging.Level,
	registerer prometheus.Registerer,
	cfg *config.Config,
) (*AppRequestNetwork, error) {
	logger := logging.NewLogger(
		"signature-aggregator-p2p",
		logging.NewWrappedCore(
			logLevel,
			os.Stdout,
			logging.JSON.ConsoleEncoder(),
		),
	)

	// Create the handler for handling inbound app responses
	handler, err := libPeers.NewRelayerExternalHandler(logger, registerer)
	if err != nil {
		logger.Error(
			"Failed to create p2p network handler",
			zap.Error(err),
		)
		return nil, err
	}

	infoAPI, err := libPeers.NewInfoAPI(cfg.InfoAPI)
	if err != nil {
		logger.Error(
			"Failed to create info API",
			zap.Error(err),
		)
		return nil, err
	}
	networkID, err := infoAPI.GetNetworkID(context.Background())
	if err != nil {
		logger.Error(
			"Failed to get network ID",
			zap.Error(err),
		)
		return nil, err
	}

	var trackedSubnets set.Set[ids.ID]
	for _, sourceBlockchain := range cfg.SourceBlockchains {
		trackedSubnets.Add(sourceBlockchain.GetSubnetID())
	}

	testNetwork, err := network.NewTestNetwork(logger, networkID, snowVdrs.NewManager(), trackedSubnets, handler)
	if err != nil {
		logger.Error(
			"Failed to create test network",
			zap.Error(err),
		)
		return nil, err
	}

	validatorClient := validators.NewCanonicalValidatorClient(logger, cfg.PChainAPI)

	arNetwork := &AppRequestNetwork{
		network:         testNetwork,
		handler:         handler,
		infoAPI:         infoAPI,
		logger:          logger,
		lock:            new(sync.Mutex),
		validatorClient: validatorClient,
	}

	// Manually connect to the validators of each of the source subnets.
	// We return an error if we are unable to connect to sufficient stake on any of the subnets.
	// Sufficient stake is determined by the Warp quora of the configured supported destinations,
	// or if the subnet supports all destinations, by the quora of all configured destinations.
	for _, sourceBlockchain := range cfg.SourceBlockchains {
		if err := arNetwork.connectToNonPrimaryNetworkPeers(cfg, sourceBlockchain); err != nil {
			return nil, err
		}
	}

	go logger.RecoverAndPanic(func() {
		testNetwork.Dispatch()
	})

	return arNetwork, nil
}

func (n *AppRequestNetwork) GetSubnetID(blockchainID ids.ID) (ids.ID, error) {
	return n.validatorClient.GetSubnetID(context.Background(), blockchainID)
}

// ConnectPeers connects the network to peers with the given nodeIDs.
// Returns the set of nodeIDs that were successfully connected to.
func (n *AppRequestNetwork) ConnectPeers(nodeIDs set.Set[ids.NodeID]) set.Set[ids.NodeID] {
	n.lock.Lock()
	defer n.lock.Unlock()

	// First, check if we are already connected to all the peers
	connectedPeers := n.network.PeerInfo(nodeIDs.List())
	if len(connectedPeers) == nodeIDs.Len() {
		return nodeIDs
	}

	// If we are not connected to all the peers already, then we have to iterate
	// through the full list of peers obtained from the info API. Rather than iterating
	// through connectedPeers for already tracked peers, just iterate through the full list,
	// re-adding connections to already tracked peers.

	// Get the list of peers
	peers, err := n.infoAPI.Peers(context.Background())
	if err != nil {
		n.logger.Error(
			"Failed to get peers",
			zap.Error(err),
		)
		return nil
	}

	// Attempt to connect to each peer
	var trackedNodes set.Set[ids.NodeID]
	for _, peer := range peers {
		if nodeIDs.Contains(peer.ID) {
			trackedNodes.Add(peer.ID)
			n.network.ManuallyTrack(peer.ID, peer.PublicIP)
			if len(trackedNodes) == nodeIDs.Len() {
				return trackedNodes
			}
		}
	}

	// If the Info API node is in nodeIDs, it will not be reflected in the call to info.Peers.
	// In this case, we need to manually track the API node.
	// if apiNodeID, _, err := n.infoAPI.GetNodeID(context.Background()); err != nil {
	// 	n.logger.Error(
	// 		"Failed to get API Node ID",
	// 		zap.Error(err),
	// 	)
	// } else if nodeIDs.Contains(apiNodeID) {
	// 	if apiNodeIP, err := n.infoAPI.GetNodeIP(context.Background()); err != nil {
	// 		n.logger.Error(
	// 			"Failed to get API Node IP",
	// 			zap.Error(err),
	// 		)
	// 	} else if ipPort, err := ips.ToIPPort(apiNodeIP); err != nil {
	// 		n.logger.Error(
	// 			"Failed to parse API Node IP",
	// 			zap.String("nodeIP", apiNodeIP),
	// 			zap.Error(err),
	// 		)
	// 	} else {
	// 		trackedNodes.Add(apiNodeID)
	// 		n.network.ManuallyTrack(apiNodeID, ipPort)
	// 	}
	// }

	return trackedNodes
}

// ConnectToCanonicalValidators connects to the canonical validators of the given subnet and returns the connected
// validator information
func (n *AppRequestNetwork) ConnectToCanonicalValidators(subnetID ids.ID) (*libPeers.ConnectedCanonicalValidators, error) {
	// Get the subnet's current canonical validator set
	validatorSet, totalValidatorWeight, err := n.validatorClient.GetCurrentCanonicalValidatorSet(subnetID)
	if err != nil {
		return nil, err
	}

	// We make queries to node IDs, not unique validators as represented by a BLS pubkey, so we need this map to track
	// responses from nodes and populate the signatureMap with the corresponding validator signature
	// This maps node IDs to the index in the canonical validator set
	nodeValidatorIndexMap := make(map[ids.NodeID]int)
	for i, vdr := range validatorSet {
		for _, node := range vdr.NodeIDs {
			nodeValidatorIndexMap[node] = i
		}
	}

	// Manually connect to all peers in the validator set
	// If new peers are connected, AppRequests may fail while the handshake is in progress.
	// In that case, AppRequests to those nodes will be retried in the next iteration of the retry loop.
	nodeIDs := set.NewSet[ids.NodeID](len(nodeValidatorIndexMap))
	for node := range nodeValidatorIndexMap {
		nodeIDs.Add(node)
	}
	connectedNodes := n.ConnectPeers(nodeIDs)

	// Check if we've connected to a stake threshold of nodes
	connectedWeight := uint64(0)
	for node := range connectedNodes {
		connectedWeight += validatorSet[nodeValidatorIndexMap[node]].Weight
	}
	return &libPeers.ConnectedCanonicalValidators{
		ConnectedWeight:       connectedWeight,
		TotalValidatorWeight:  totalValidatorWeight,
		ValidatorSet:          validatorSet,
		NodeValidatorIndexMap: nodeValidatorIndexMap,
	}, nil
}

func (n *AppRequestNetwork) Send(msg message.OutboundMessage, nodeIDs set.Set[ids.NodeID], subnetID ids.ID, allower subnets.Allower) set.Set[ids.NodeID] {
	return n.network.Send(msg, avagoCommon.SendConfig{NodeIDs: nodeIDs}, subnetID, allower)
}

func (n *AppRequestNetwork) RegisterAppRequest(requestID ids.RequestID) {
	n.handler.RegisterAppRequest(requestID)
}
func (n *AppRequestNetwork) RegisterRequestID(requestID uint32, numExpectedResponse int) chan message.InboundMessage {
	return n.handler.RegisterRequestID(requestID, numExpectedResponse)
}

// Private helpers

// Connect to the validators of the source blockchain. For each destination blockchain,
// verify that we have connected to a threshold of stake.
func (n *AppRequestNetwork) connectToNonPrimaryNetworkPeers(
	cfg *config.Config,
	sourceBlockchain *config.SourceBlockchain,
) error {
	subnetID := sourceBlockchain.GetSubnetID()
	_, err := n.ConnectToCanonicalValidators(subnetID)
	if err != nil {
		n.logger.Error(
			"Failed to connect to canonical validators",
			zap.String("subnetID", subnetID.String()),
			zap.Error(err),
		)
		return err
	}
	return nil
}
