package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"sync"

	"github.com/awgh/ratnet"
	"github.com/awgh/ratnet/api"
)

const (
	recentBufferSize = 8
	cacheSize        = 256
	entriesPerTable  = cacheSize / recentBufferSize
	nonceSize        = 32
)

type recentPage map[[nonceSize]byte]bool

type recentBuffer struct {
	mtx           sync.Mutex
	recentPageIdx int32
	recentBuffer  [recentBufferSize]recentPage
}

func newRecentBuffer() (r recentBuffer) {
	for i := range r.recentBuffer {
		r.recentBuffer[i] = make(recentPage, entriesPerTable)
	}
	return
}

func (r *recentBuffer) hasMsgBeenSeen(nonce [nonceSize]byte) bool {
	for i := range r.recentBuffer {
		if _, ok := r.recentBuffer[i][nonce]; ok {
			return ok
		}
	}
	return false
}

func (r *recentBuffer) resetRecentPageIfFull() bool {
	isFull := len(r.recentBuffer[r.recentPageIdx]) >= entriesPerTable

	if isFull {
		r.recentBuffer[r.recentPageIdx] = make(recentPage, entriesPerTable)
	}
	return isFull
}

func (r *recentBuffer) setMsgSeen(nonce [nonceSize]byte) {
	r.recentBuffer[r.recentPageIdx][nonce] = true
}

// seenRecently : Returns whether this message should be filtered out by loop detection
func (r *recentBuffer) seenRecently(nonce []byte) bool {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	if len(nonce) != nonceSize {
		log.Fatalf("invalid nonce size %d", len(nonce))
	}

	var nonceVal [nonceSize]byte
	copy(nonceVal[:], nonce[:nonceSize])

	seen := r.hasMsgBeenSeen(nonceVal)

	if reset := r.resetRecentPageIfFull(); reset {
		if r.recentPageIdx < recentBufferSize-1 {
			r.recentPageIdx++
		} else {
			r.recentPageIdx = 0
		}
	}

	if !seen {
		r.setMsgSeen(nonceVal)
	}

	return seen
}

// DefaultRouter - The Default router makes no changes at all,
//                 every message is sent out on the same channel it came in on,
//                 and non-channel messages are consumed but not forwarded
type DefaultRouter struct {
	// Internal
	recentBuffer

	Patches []api.Patch

	// Configuration Settings

	// CheckContent - Check if incoming messages are for the contentKey
	CheckContent bool
	// CheckChannels - Check if incoming messages are for any of the channel keys
	CheckChannels bool
	// CheckProfiles - Check if incoming messages are for any of the profile keys
	CheckProfiles bool

	// ForwardConsumedContent - Should node forward consumed messages that matched contentKey
	ForwardConsumedContent bool
	// ForwardConsumedContent - Should node forward consumed messages that matched a channel key
	ForwardConsumedChannels bool
	// ForwardConsumedProfile - Should node forward consumed messages that matched a profile key
	ForwardConsumedProfiles bool

	// ForwardUnknownContent - Should node forward non-consumed messages that matched contentKey
	ForwardUnknownContent bool
	// ForwardUnknownContent - Should node forward non-consumed messages that matched a channel key
	ForwardUnknownChannels bool
	// ForwardUnknownProfile - Should node forward non-consumed messages that matched a profile key
	ForwardUnknownProfiles bool
}

func init() {
	ratnet.Routers["default"] = NewRouterFromMap // register this module by name (for deserialization support)
}

// NewRouterFromMap : Makes a new instance of this module from a map of arguments (for deserialization support)
func NewRouterFromMap(r map[string]interface{}) api.Router {
	return NewDefaultRouter()
}

// NewDefaultRouter - returns a new instance of DefaultRouter
func NewDefaultRouter() *DefaultRouter {
	r := new(DefaultRouter)
	r.CheckContent = true
	r.CheckChannels = true
	r.CheckProfiles = false
	r.ForwardUnknownContent = true
	r.ForwardUnknownChannels = true
	r.ForwardUnknownProfiles = false
	r.ForwardConsumedContent = false
	r.ForwardConsumedChannels = true
	r.ForwardConsumedProfiles = false
	// init page maps
	r.recentBuffer = newRecentBuffer()
	return r
}

// Patch : Redirect messages from one input to different outputs
func (r *DefaultRouter) Patch(patch api.Patch) {
	r.Patches = append(r.Patches, patch)
}

// GetPatches : Returns an array with the mappings of incoming channels to destination channels
func (r *DefaultRouter) GetPatches() []api.Patch {
	return r.Patches
}

func (r *DefaultRouter) forward(node api.Node, msg api.Msg) error {
	for _, p := range r.Patches { //todo: this could be constant-time
		if msg.Name == p.From { // we don't check for IsChan here, we allow forwarding from "" chan to channels
			for i := 0; i < len(p.To); i++ {
				msg.Name = p.To[i]
				if msg.Name == "" {
					msg.IsChan = false
				} else {
					msg.IsChan = true
				}
				if err := node.Forward(msg); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if err := node.Forward(msg); err != nil {
		return err
	}
	return nil
}

// Route - Router that does default behavior
func (r *DefaultRouter) Route(node api.Node, message []byte) error {

	//  Stuff Everything will need just about every time...
	//
	var msg api.Msg
	flags := message[0]
	idx := 1
	msg.IsChan = ((flags & api.ChannelFlag) != 0)
	msg.Chunked = ((flags & api.ChunkedFlag) != 0)
	msg.StreamHeader = ((flags & api.StreamHeaderFlag) != 0)
	var channelLen uint16 // beginning uint16 of message is channel name length
	if msg.IsChan {
		channelLen = (uint16(message[1]) << 8) | uint16(message[2])
		/*
			if len(message) < int(channelLen)+1+2+56 { //+64 { // flags + uint16 + LuggageTag

				log.Println(message)
				return errors.New("Incorrect channel name length")
			}
		*/
		msg.Name = string(message[3 : 3+channelLen]) // flags[0], chan name length[1,2]
		idx += 2 + int(channelLen)                   //skip over the channel name
	}
	if idx+16 >= len(message) {
		log.Println(message)
		return errors.New("Malformed message")
	}
	nonce := message[idx : idx+nonceSize]
	if r.seenRecently(nonce) { // LOOP PREVENTION before handling or forwarding
		return nil
	}
	cid, err := node.CID() // we need this for cloning
	if err != nil {
		return err
	}
	msg.Content = bytes.NewBuffer(message[idx:])

	// Routing Logic
	if msg.IsChan { // channel message
		consumed := false
		if r.CheckChannels {
			chn, err := node.GetChannel(msg.Name)
			if chn != nil && err == nil { // this is a channel key we know
				pubkey := cid.Clone()
				pubkey.FromB64(chn.Pubkey)
				consumed, err = node.Handle(msg)
				if err != nil {
					return err
				}
			}
		}
		if (!consumed && r.ForwardUnknownChannels) || (consumed && r.ForwardConsumedChannels) {
			if err := r.forward(node, msg); err != nil {
				return err
			}
		}
	} else { // private message (zero length channel)
		// content key case (to be removed, deprecated)
		consumed := false
		if r.CheckContent {
			consumed, err = node.Handle(msg)
			if err != nil {
				return err
			}
		}
		if (!consumed && r.ForwardUnknownContent) || (consumed && r.ForwardConsumedContent) {
			if err := r.forward(node, msg); err != nil {
				return err
			}
		}

		// profile keys case
		consumed = false
		if r.CheckProfiles {
			profiles, err := node.GetProfiles()
			if err != nil {
				return err
			}
			for _, profile := range profiles {
				if !profile.Enabled {
					continue
				}
				pubkey := cid.Clone()
				pubkey.FromB64(profile.Pubkey)
				consumed, err = node.Handle(msg)
				if err != nil {
					return err
				}
				if consumed {
					break
				}
			}
		}
		if (!consumed && r.ForwardUnknownProfiles) || (consumed && r.ForwardConsumedProfiles) {
			if err := r.forward(node, msg); err != nil {
				return err
			}
		}
	}
	return nil
}

// MarshalJSON : Create a serialized JSON blob out of the config of this router
func (r *DefaultRouter) MarshalJSON() (b []byte, e error) {

	return json.Marshal(map[string]interface{}{
		"Router":                  "default",
		"CheckContent":            r.CheckContent,
		"ForwardConsumedContent":  r.ForwardConsumedContent,
		"ForwardUnknownContent":   r.ForwardUnknownContent,
		"CheckProfiles":           r.CheckProfiles,
		"ForwardConsumedProfiles": r.ForwardConsumedProfiles,
		"ForwardUnknownProfiles":  r.ForwardUnknownProfiles,
		"CheckChannels":           r.CheckChannels,
		"ForwardConsumedChannels": r.ForwardConsumedChannels,
		"ForwardUnknownChannels":  r.ForwardUnknownChannels,
		"Patches":                 r.Patches})
}
