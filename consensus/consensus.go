// Consensus package implements the Cosi PBFT consensus
package consensus // consensus

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/dedis/kyber"
	"github.com/dedis/kyber/sign/schnorr"
	"github.com/simple-rules/harmony-benchmark/blockchain"
	"github.com/simple-rules/harmony-benchmark/crypto"
	"github.com/simple-rules/harmony-benchmark/crypto/pki"
	"github.com/simple-rules/harmony-benchmark/log"
	"github.com/simple-rules/harmony-benchmark/p2p"
	"github.com/simple-rules/harmony-benchmark/utils"
)

// Consensus data containing all info related to one round of consensus process
type Consensus struct {
	state ConsensusState
	// Commits collected from validators. A map from node Id to its commitment
	commitments               *map[uint16]kyber.Point
	finalCommitments          *map[uint16]kyber.Point
	aggregatedCommitment      kyber.Point
	aggregatedFinalCommitment kyber.Point
	bitmap                    *crypto.Mask
	finalBitmap               *crypto.Mask

	// Challenges
	challenge      [32]byte
	finalChallenge [32]byte

	// Responses collected from validators
	responses      *map[uint16]kyber.Scalar
	finalResponses *map[uint16]kyber.Scalar
	// map of nodeID to validator Peer object
	validators map[uint16]p2p.Peer
	// Leader
	leader p2p.Peer
	// Public keys of the committee including leader and validators
	publicKeys []kyber.Point

	// private/public keys of current node
	priKey kyber.Scalar
	pubKey kyber.Point

	// Whether I am leader. False means I am validator
	IsLeader bool
	// Leader or validator Id - 2 byte
	nodeID uint16
	// Consensus Id (View Id) - 4 byte
	consensusID uint32
	// Blockhash - 32 byte
	blockHash [32]byte
	// BlockHeader to run consensus on
	blockHeader []byte
	// Array of block hashes.
	blockHashes [][32]byte
	// Shard Id which this node belongs to
	ShardID uint32

	// global consensus mutex
	mutex sync.Mutex

	// Validator specific fields
	// Blocks received but not done with consensus yet
	blocksReceived map[uint32]*BlockConsensusStatus
	// Commitment secret
	secret map[uint32]kyber.Scalar

	// Signal channel for starting a new consensus process
	ReadySignal chan struct{}
	// The verifier func passed from Node object
	BlockVerifier func(*blockchain.Block) bool
	// The post-consensus processing func passed from Node object
	// Called when consensus on a new block is done
	OnConsensusDone func(*blockchain.Block)

	Log log.Logger
}

// This used to keep track of the consensus status of multiple blocks received so far
// This is mainly used in the case that this node is lagging behind and needs to catch up.
// For example, the consensus moved to round N and this node received message(N).
// However, this node may still not finished with round N-1, so the newly received message(N)
// should be stored in this temporary structure. In case the round N-1 finishes, it can catch
// up to the latest state of round N by using this structure.
type BlockConsensusStatus struct {
	blockHeader []byte         // the block header of the block which the consensus is running on
	state       ConsensusState // the latest state of the consensus
}

// NewConsensus creates a new Consensus object
// TODO(minhdoan): Maybe convert it into just New
// FYI, see https://golang.org/doc/effective_go.html?#package-names
func NewConsensus(ip, port, ShardID string, peers []p2p.Peer, leader p2p.Peer) *Consensus {
	consensus := Consensus{}

	if leader.Port == port && leader.Ip == ip {
		consensus.IsLeader = true
	} else {
		consensus.IsLeader = false
	}

	consensus.commitments = &map[uint16]kyber.Point{}
	consensus.finalCommitments = &map[uint16]kyber.Point{}
	consensus.validators = make(map[uint16]p2p.Peer)
	consensus.responses = &map[uint16]kyber.Scalar{}
	consensus.finalResponses = &map[uint16]kyber.Scalar{}

	consensus.leader = leader
	for _, peer := range peers {
		consensus.validators[utils.GetUniqueIdFromPeer(peer)] = peer
	}

	// Initialize cosign bitmap
	allPublicKeys := make([]kyber.Point, 0)
	for _, validatorPeer := range peers {
		allPublicKeys = append(allPublicKeys, validatorPeer.PubKey)
	}
	allPublicKeys = append(allPublicKeys, leader.PubKey)
	mask, err := crypto.NewMask(crypto.Ed25519Curve, allPublicKeys, consensus.leader.PubKey)
	if err != nil {
		panic("Failed to create mask")
	}
	finalMask, err := crypto.NewMask(crypto.Ed25519Curve, allPublicKeys, consensus.leader.PubKey)
	if err != nil {
		panic("Failed to create final mask")
	}
	consensus.publicKeys = allPublicKeys
	consensus.bitmap = mask
	consensus.finalBitmap = finalMask

	consensus.secret = map[uint32]kyber.Scalar{}

	// For now use socket address as 16 byte Id
	// TODO: populate with correct Id
	consensus.nodeID = utils.GetUniqueIdFromPeer(p2p.Peer{Ip: ip, Port: port})

	// Set private key for myself so that I can sign messages.
	consensus.priKey = crypto.Ed25519Curve.Scalar().SetInt64(int64(consensus.nodeID))
	consensus.pubKey = pki.GetPublicKeyFromScalar(consensus.priKey)
	consensus.consensusID = 0 // or view Id in the original pbft paper

	myShardID, err := strconv.Atoi(ShardID)
	if err != nil {
		panic("Unparseable shard Id" + ShardID)
	}
	consensus.ShardID = uint32(myShardID)

	// For validators to keep track of all blocks received but not yet committed, so as to catch up to latest consensus if lagged behind.
	consensus.blocksReceived = make(map[uint32]*BlockConsensusStatus)

	if consensus.IsLeader {
		consensus.ReadySignal = make(chan struct{})
		// send a signal to indicate it's ready to run consensus
		// this signal is consumed by node object to create a new block and in turn trigger a new consensus on it
		// this is a goroutine because go channel without buffer will block
		go func() {
			consensus.ReadySignal <- struct{}{}
		}()
	}

	consensus.Log = log.New()
	return &consensus
}

func (consensus *Consensus) signMessage(message []byte) []byte {
	signature, err := schnorr.Sign(crypto.Ed25519Curve, consensus.priKey, message)
	if err != nil {
		panic("Failed to sign message with Schnorr signature.")
	}
	return signature
}

func (consensus *Consensus) GetValidatorPeers() []p2p.Peer {
	validatorPeers := make([]p2p.Peer, 0)
	for _, validatorPeer := range consensus.validators {
		validatorPeers = append(validatorPeers, validatorPeer)
	}
	return validatorPeers
}

// Reset the state of the consensus
func (consensus *Consensus) ResetState() {
	consensus.state = Finished
	consensus.commitments = &map[uint16]kyber.Point{}
	consensus.finalCommitments = &map[uint16]kyber.Point{}
	consensus.responses = &map[uint16]kyber.Scalar{}
	consensus.finalResponses = &map[uint16]kyber.Scalar{}

	mask, _ := crypto.NewMask(crypto.Ed25519Curve, consensus.publicKeys, consensus.leader.PubKey)
	finalMask, _ := crypto.NewMask(crypto.Ed25519Curve, consensus.publicKeys, consensus.leader.PubKey)
	consensus.bitmap = mask
	consensus.finalBitmap = finalMask
	consensus.bitmap.SetMask([]byte{})
	consensus.finalBitmap.SetMask([]byte{})

	consensus.aggregatedCommitment = nil
	consensus.aggregatedFinalCommitment = nil
	consensus.secret = map[uint32]kyber.Scalar{}
}

// Returns a string representation of this consensus
func (consensus *Consensus) String() string {
	var duty string
	if consensus.IsLeader {
		duty = "LDR" // leader
	} else {
		duty = "VLD" // validator
	}
	return fmt.Sprintf("[duty:%s, priKey:%s, ShardID:%v, nodeID:%v, state:%s]",
		duty, consensus.priKey.String(), consensus.ShardID, consensus.nodeID, consensus.state)
}
