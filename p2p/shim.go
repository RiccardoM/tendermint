package p2p

import (
	"fmt"

	"github.com/gogo/protobuf/proto"
)

var _ Reactor = (*ReactorShim)(nil)

type (
	// ReactorShim defines a generic shim wrapper around a BaseReactor. It is
	// responsible for wiring up legacy p2p behavior to the new p2p semantics
	// (e.g. proxying Envelope messages to legacy peers).
	ReactorShim struct {
		BaseReactor

		Name             string
		PeerUpdateCh     chan PeerUpdate
		Channels         map[ChannelID]*ChannelShim
		MessageValidator MessageValidator
	}

	MessageValidator interface {
		OnUnmarshalFailure(chID byte, src Peer, msgBytes []byte, err error)
		Validate(chID byte, src Peer, msgBytes []byte, msg proto.Message) error
	}

	// ChannelShim defines a generic shim wrapper around a legacy p2p channel
	// and the new p2p Channel. It also includes the raw bi-directional Go channels
	// so we can proxy message delivery.
	ChannelShim struct {
		Descriptor *ChannelDescriptor
		Channel    *Channel
		InCh       chan Envelope
		OutCh      chan Envelope
		PeerErrCh  chan PeerError
	}

	// ChannelDescriptorShim defines a shim wrapper around a legacy p2p channel
	// and the proto.Message the new p2p Channel is responsible for handling.
	// A ChannelDescriptorShim is not contained in ReactorShim, but is rather
	// used to construct a ReactorShim.
	ChannelDescriptorShim struct {
		MsgType    proto.Message
		Descriptor *ChannelDescriptor
	}
)

func NewShim(name string, impl Reactor, descriptors []*ChannelDescriptorShim, msgVal MessageValidator) *ReactorShim {
	br := *NewBaseReactor(name, impl)

	channels := make(map[ChannelID]*ChannelShim)
	for _, cds := range descriptors {
		cID := ChannelID(cds.Descriptor.ID)
		inCh := make(chan Envelope)
		outCh := make(chan Envelope)
		peerErrCh := make(chan PeerError)

		channels[cID] = &ChannelShim{
			Descriptor: cds.Descriptor,
			Channel:    NewChannel(cID, cds.MsgType, inCh, outCh, peerErrCh),
			InCh:       inCh,
			OutCh:      outCh,
			PeerErrCh:  peerErrCh,
		}
	}

	return &ReactorShim{
		BaseReactor:      br,
		Name:             name,
		PeerUpdateCh:     make(chan PeerUpdate),
		Channels:         channels,
		MessageValidator: msgVal,
	}
}

// proxyPeerEnvelopes iterates over each p2p Channel and starts a separate
// go-routine where we listen for outbound envelopes sent during Receive
// executions (or anything else that may send on the Channel) and proxy them to
// the coressponding Peer using the To field from the envelope.
func (rs *ReactorShim) proxyPeerEnvelopes() {
	for _, c := range rs.Channels {
		go func(chID byte, outCh chan Envelope) {
			for e := range outCh {
				src := rs.Switch.peers.Get(ID(e.To.String()))
				if src == nil {
					panic(fmt.Sprintf("failed to proxy envelope; failed to find peer (%s)", e.To))
				}

				bz, err := proto.Marshal(e.Message)
				if err != nil {
					panic(fmt.Sprintf("failed to proxy envelope; failed to encode message: %s", err))
				}

				_ = src.Send(chID, bz)
			}
		}(c.Descriptor.ID, c.OutCh)
	}
}

func (rs *ReactorShim) GetChannels() []*ChannelDescriptor {
	descriptors := make([]*ChannelDescriptor, len(rs.Channels))
	i := 0

	for _, c := range rs.Channels {
		descriptors[i] = c.Descriptor
		i++
	}

	return descriptors
}

// OnStart executes the reactor shim's OnStart hook where we start all the
// necessary go-routines in order to proxy peer errors and messages.
func (rs *ReactorShim) OnStart() error {
	if rs.IsRunning() {
		rs.proxyPeerEnvelopes()
	}

	return nil
}

// OnStop executes the reactor shim's OnStop hook where all p2p Channels are
// closed and the PeerUpdateCh is closed.
func (rs *ReactorShim) OnStop() {
	for _, cs := range rs.Channels {
		if err := cs.Channel.Close(); err != nil {
			rs.Logger.Error("failed to close channel", "reactor", rs.Name, "ch_id", cs.Channel.ID, "err", err)
		}
	}

	close(rs.PeerUpdateCh)
}

// AddPeer sends a PeerUpdate with status PeerStatusUp on the PeerUpdateCh.
// The embedding reactor must be sure to listen for messages on this channel to
// handle adding a peer.
func (rs *ReactorShim) AddPeer(peer Peer) {
	peerID, err := PeerIDFromString(string(peer.ID()))
	if err != nil {
		// It is OK to panic here as we'll be removing the Reactor interface and
		// Peer type in favor of using a PeerID directly.
		panic(err)
	}

	select {
	case rs.PeerUpdateCh <- PeerUpdate{PeerID: peerID, Status: PeerStatusUp}:
		rs.Logger.Debug("sent peer update", "reactor", rs.Name, "peer", peerID.String(), "status", PeerStatusUp)

	default:
		rs.Logger.Debug("dropped peer update", "reactor", rs.Name, "peer", peerID.String(), "status", PeerStatusUp)
	}
}

// RemovePeer sends a PeerUpdate with status PeerStatusDown on the PeerUpdateCh.
// The embedding reactor must be sure to listen for messages on this channel to
// handle removing a peer.
func (rs *ReactorShim) RemovePeer(peer Peer, reason interface{}) {
	peerID, err := PeerIDFromString(string(peer.ID()))
	if err != nil {
		// It is OK to panic here as we'll be removing the Reactor interface and
		// Peer type in favor of using a PeerID directly.
		panic(err)
	}

	select {
	case rs.PeerUpdateCh <- PeerUpdate{PeerID: peerID, Status: PeerStatusDown}:
		rs.Logger.Debug("sent peer update", "reactor", rs.Name, "peer", peerID.String(), "status", PeerStatusDown)

	default:
		rs.Logger.Debug("dropped peer update", "reactor", rs.Name, "peer", peerID.String(), "status", PeerStatusDown)
	}
}

// Receive implements a generic wrapper around implementing the Receive method
// on the legacy Reactor p2p interface. If the reactor is running, Receive will
// find the corresponding new p2p Channel, create and decode the appropriate
// proto.Message from the msgBytes, execute any validation and finally construct
// and send a p2p Envelope on the appropriate p2p Channel.
func (rs *ReactorShim) Receive(chID byte, src Peer, msgBytes []byte) {
	if !rs.IsRunning() {
		return
	}

	cID := ChannelID(chID)
	channelShim, ok := rs.Channels[cID]
	if !ok {
		rs.Logger.Error("unexpected channel", "peer", src, "ch_id", chID)
		return
	}

	msg := proto.Clone(channelShim.Channel.messageType)
	msg.Reset()

	if err := proto.Unmarshal(msgBytes, msg); err != nil {
		rs.Logger.Error("error decoding message", "peer", src, "ch_id", cID, "msg", msg, "err", err)
		if rs.MessageValidator != nil {
			rs.MessageValidator.OnUnmarshalFailure(chID, src, msgBytes, err)
		}

		return
	}

	if rs.MessageValidator != nil {
		if err := rs.MessageValidator.Validate(chID, src, msgBytes, msg); err != nil {
			rs.Logger.Error("invalid message", "peer", src, "ch_id", cID, "msg", msg, "err", err)
			return
		}
	}

	peerID, err := PeerIDFromString(string(src.ID()))
	if err != nil {
		// It is OK to panic here as we'll be removing the Reactor interface and
		// Peer type in favor of using a PeerID directly.
		panic(err)
	}

	select {
	case channelShim.InCh <- Envelope{From: peerID, Message: msg}:
		rs.Logger.Debug("proxied envelope", "reactor", rs.Name, "ch_id", cID, "peer", peerID.String())

	default:
		rs.Logger.Debug("dropped envelope", "reactor", rs.Name, "ch_id", cID, "peer", peerID.String())
	}
}
