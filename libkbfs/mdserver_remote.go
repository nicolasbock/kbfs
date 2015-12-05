package libkbfs

import (
	"fmt"
	"sync"
	"time"

	"github.com/keybase/go-framed-msgpack-rpc"

	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

const (
	// MdServerTokenServer is the expected server type for mdserver authentication.
	MdServerTokenServer = "kbfs_md"
	// MdServerTokenExpireIn is the TTL to use when constructing an authentication token.
	MdServerTokenExpireIn = 2 * 60 * 60 // 2 hours
	// MdServerClientName is the client name to include in an authentication token.
	MdServerClientName = "libkbfs_mdserver_remote"
	// MdServerClientVersion is the client version to include in an authentication token.
	MdServerClientVersion = Version + "-" + DefaultBuild
)

// MDServerRemote is an implementation of the MDServer interface.
type MDServerRemote struct {
	config    Config
	conn      *Connection
	client    keybase1.MetadataClient
	log       logger.Logger
	authToken *AuthToken

	observerMu sync.Mutex // protects observers
	observers  map[TlfID]chan<- error

	tickerCancel context.CancelFunc
	tickerMu     sync.Mutex // protects the ticker cancel function
}

// Test that MDServerRemote fully implements the MDServer interface.
var _ MDServer = (*MDServerRemote)(nil)

// Test that MDServerRemote fully implements the KeyServer interface.
var _ KeyServer = (*MDServerRemote)(nil)

// Test that MDServerRemote fully implements the AuthTokenRefreshHandler interface.
var _ AuthTokenRefreshHandler = (*MDServerRemote)(nil)

// NewMDServerRemote returns a new instance of MDServerRemote.
func NewMDServerRemote(config Config, srvAddr string) *MDServerRemote {
	mdServer := &MDServerRemote{
		config:    config,
		observers: make(map[TlfID]chan<- error),
		log:       config.MakeLogger(""),
	}
	mdServer.authToken = NewAuthToken(config,
		MdServerTokenServer, MdServerTokenExpireIn,
		MdServerClientName, MdServerClientVersion, mdServer)
	conn := NewTLSConnection(config, srvAddr, MDServerErrorUnwrapper{}, mdServer, true)
	mdServer.conn = conn
	mdServer.client = keybase1.MetadataClient{Cli: conn.GetClient()}
	return mdServer
}

// OnConnect implements the ConnectionHandler interface.
func (md *MDServerRemote) OnConnect(ctx context.Context,
	conn *Connection, client keybase1.GenericClient,
	server *rpc.Server) error {

	// get a new signature
	signature, err := md.authToken.Sign(ctx)
	if err != nil {
		return err
	}

	// authenticate -- using md.client here would cause problematic recursion.
	c := keybase1.MetadataClient{Cli: cancelableClient{client}}
	pingIntervalSeconds, err := c.Authenticate(ctx, signature)
	if err != nil {
		return err
	}

	// request a list of folders needing rekey action
	if err := md.getFoldersForRekey(ctx, c, server); err != nil {
		md.log.Warning("MDServerRemote: getFoldersForRekey failed with %v", err)
	}

	// start pinging
	md.resetPingTicker(pingIntervalSeconds)
	return nil
}

// RefreshAuthToken implements the AuthTokenRefreshHandler interface.
func (md *MDServerRemote) RefreshAuthToken(ctx context.Context) {
	// get a new signature
	signature, err := md.authToken.Sign(ctx)
	if err != nil {
		md.log.Debug("MDServerRemote: error signing auth token: %v", err)
	}
	// update authentication
	if _, err := md.client.Authenticate(ctx, signature); err != nil {
		md.log.Debug("MDServerRemote: error refreshing auth token: %v", err)
	}
}

// Helper to reset a ping ticker.
func (md *MDServerRemote) resetPingTicker(intervalSeconds int) {
	md.tickerMu.Lock()
	defer md.tickerMu.Unlock()

	if md.tickerCancel != nil {
		md.tickerCancel()
		md.tickerCancel = nil
	}
	if intervalSeconds <= 0 {
		return
	}

	md.log.Debug("MDServerRemote: starting new ping ticker with interval %d",
		intervalSeconds)

	var ctx context.Context
	ctx, md.tickerCancel = context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
		for {
			select {
			case <-ticker.C:
				err := md.client.Ping(ctx)
				if err != nil {
					md.log.Debug("MDServerRemote: ping error %s", err)
				}

			case <-ctx.Done():
				md.log.Debug("MDServerRemote: stopping ping ticker")
				ticker.Stop()
				return
			}
		}
	}()
}

// OnConnectError implements the ConnectionHandler interface.
func (md *MDServerRemote) OnConnectError(err error, wait time.Duration) {
	md.log.Warning("MDServerRemote: connection error: %q; retrying in %s",
		err, wait)
	// TODO: it might make sense to show something to the user if this is
	// due to authentication, for example.
	md.cancelObservers()
	md.resetPingTicker(0)
	if md.authToken != nil {
		md.authToken.Shutdown()
	}
}

// OnDoCommandError implements the ConnectionHandler interface.
func (md *MDServerRemote) OnDoCommandError(err error, wait time.Duration) {
	md.log.Warning("MDServerRemote: DoCommand error: %q; retrying in %s",
		err, wait)
}

// OnDisconnected implements the ConnectionHandler interface.
func (md *MDServerRemote) OnDisconnected(status DisconnectStatus) {
	if status == StartingNonFirstConnection {
		md.log.Warning("MDServerRemote is disconnected")
	}
	md.cancelObservers()
	md.resetPingTicker(0)
	if md.authToken != nil {
		md.authToken.Shutdown()
	}
}

// ShouldThrottle implements the ConnectionHandler interface.
func (md *MDServerRemote) ShouldThrottle(err error) bool {
	if err == nil {
		return false
	}
	_, shouldThrottle := err.(MDServerErrorThrottle)
	return shouldThrottle
}

// Signal errors and clear any registered observers.
func (md *MDServerRemote) cancelObservers() {
	md.observerMu.Lock()
	defer md.observerMu.Unlock()
	// fire errors for any registered observers
	for id, observerChan := range md.observers {
		md.signalObserverLocked(observerChan, id, MDServerDisconnected{})
	}
}

// Signal an observer. The observer lock must be held.
func (md *MDServerRemote) signalObserverLocked(observerChan chan<- error, id TlfID, err error) {
	observerChan <- err
	close(observerChan)
	delete(md.observers, id)
}

// Helper used to retrieve metadata blocks from the MD server.
func (md *MDServerRemote) get(ctx context.Context, id TlfID, handle *TlfHandle,
	bid BranchID, mStatus MergeStatus, start, stop MetadataRevision) (
	TlfID, []*RootMetadataSigned, error) {
	// figure out which args to send
	if id == NullTlfID && handle == nil {
		return id, nil, MDInvalidGetArguments{
			id:     id,
			handle: handle,
		}
	}
	arg := keybase1.GetMetadataArg{
		StartRevision: start.Number(),
		StopRevision:  stop.Number(),
		BranchID:      bid.String(),
		Unmerged:      mStatus == Unmerged,
		LogTags:       LogTagsFromContextToMap(ctx),
	}
	if id == NullTlfID {
		arg.FolderHandle = handle.ToBytes(md.config)
	} else {
		arg.FolderID = id.String()
	}

	// request
	response, err := md.client.GetMetadata(ctx, arg)
	if err != nil {
		return id, nil, err
	}

	// response
	id = ParseTlfID(response.FolderID)
	if id == NullTlfID {
		return id, nil, MDInvalidTlfID{response.FolderID}
	}

	// deserialize blocks
	rmdses := make([]*RootMetadataSigned, len(response.MdBlocks))
	for i := range response.MdBlocks {
		var rmds RootMetadataSigned
		err = md.config.Codec().Decode(response.MdBlocks[i], &rmds)
		if err != nil {
			return id, rmdses, err
		}
		rmdses[i] = &rmds
	}
	return id, rmdses, nil
}

// GetForHandle implements the MDServer interface for MDServerRemote.
func (md *MDServerRemote) GetForHandle(ctx context.Context, handle *TlfHandle,
	mStatus MergeStatus) (TlfID, *RootMetadataSigned, error) {
	id, rmdses, err := md.get(ctx, NullTlfID, handle, NullBranchID, mStatus,
		MetadataRevisionUninitialized, MetadataRevisionUninitialized)
	if err != nil {
		return id, nil, err
	}
	if len(rmdses) == 0 {
		return id, nil, nil
	}
	return id, rmdses[0], nil
}

// GetForTLF implements the MDServer interface for MDServerRemote.
func (md *MDServerRemote) GetForTLF(ctx context.Context, id TlfID,
	bid BranchID, mStatus MergeStatus) (*RootMetadataSigned, error) {
	_, rmdses, err := md.get(ctx, id, nil, bid, mStatus,
		MetadataRevisionUninitialized, MetadataRevisionUninitialized)
	if err != nil {
		return nil, err
	}
	if len(rmdses) == 0 {
		return nil, nil
	}
	return rmdses[0], nil
}

// GetRange implements the MDServer interface for MDServerRemote.
func (md *MDServerRemote) GetRange(ctx context.Context, id TlfID,
	bid BranchID, mStatus MergeStatus, start, stop MetadataRevision) (
	[]*RootMetadataSigned, error) {
	_, rmds, err := md.get(ctx, id, nil, bid, mStatus, start, stop)
	return rmds, err
}

// Put implements the MDServer interface for MDServerRemote.
func (md *MDServerRemote) Put(ctx context.Context, rmds *RootMetadataSigned) error {
	// encode MD block
	rmdsBytes, err := md.config.Codec().Encode(rmds)
	if err != nil {
		return err
	}

	// put request
	arg := keybase1.PutMetadataArg{
		MdBlock: rmdsBytes,
		LogTags: LogTagsFromContextToMap(ctx),
	}
	return md.client.PutMetadata(ctx, arg)
}

// PruneBranch implementms the MDServer interface for MDServerRemote.
func (md *MDServerRemote) PruneBranch(ctx context.Context, id TlfID, bid BranchID) error {
	arg := keybase1.PruneBranchArg{
		FolderID: id.String(),
		BranchID: bid.String(),
		LogTags:  LogTagsFromContextToMap(ctx),
	}
	return md.client.PruneBranch(ctx, arg)
}

// MetadataUpdate implements the MetadataUpdateProtocol interface.
func (md *MDServerRemote) MetadataUpdate(_ context.Context, arg keybase1.MetadataUpdateArg) error {
	id := ParseTlfID(arg.FolderID)
	if id == NullTlfID {
		return MDServerErrorBadRequest{"Invalid folder ID"}
	}

	md.observerMu.Lock()
	defer md.observerMu.Unlock()
	observerChan, ok := md.observers[id]
	if !ok {
		// not registered
		return nil
	}

	// signal that we've seen the update
	md.signalObserverLocked(observerChan, id, nil)
	return nil
}

// FolderNeedsRekey implements the MetadataUpdateProtocol interface.
func (md *MDServerRemote) FolderNeedsRekey(_ context.Context, arg keybase1.FolderNeedsRekeyArg) error {
	id := ParseTlfID(arg.FolderID)
	if id == NullTlfID {
		return MDServerErrorBadRequest{"Invalid folder ID"}
	}

	// TODO: send this to a rekeyer routine.
	md.log.Debug("MDServerRemote: folder needs rekey: %s", id.String())
	return nil
}

// RegisterForUpdate implements the MDServer interface for MDServerRemote.
func (md *MDServerRemote) RegisterForUpdate(ctx context.Context, id TlfID,
	currHead MetadataRevision) (<-chan error, error) {
	arg := keybase1.RegisterForUpdatesArg{
		FolderID:     id.String(),
		CurrRevision: currHead.Number(),
		LogTags:      LogTagsFromContextToMap(ctx),
	}

	// register
	var c chan error
	err := md.conn.DoCommand(ctx, func(rawClient keybase1.GenericClient) error {
		// set up the server to receive updates, since we may
		// get disconnected between retries.
		server := md.conn.GetServer()
		err := server.Register(keybase1.MetadataUpdateProtocol(md))
		if err != nil {
			if _, ok := err.(rpc.AlreadyRegisteredError); !ok {
				return err
			}
		}
		err = server.Run(true)
		if err != nil {
			return err
		}

		// keep re-adding the observer on retries, since
		// disconnects or connection errors clear observers.
		func() {
			md.observerMu.Lock()
			defer md.observerMu.Unlock()
			if _, ok := md.observers[id]; ok {
				panic(fmt.Sprintf("Attempted double-registration for folder: %s",
					id))
			}
			c = make(chan error, 1)
			md.observers[id] = c
		}()
		// Use this instead of md.client since we're already
		// inside a DoCommand().
		c := keybase1.MetadataClient{Cli: rawClient}
		err = c.RegisterForUpdates(ctx, arg)
		if err != nil {
			func() {
				md.observerMu.Lock()
				defer md.observerMu.Unlock()
				// we could've been canceled by a shutdown so look this up
				// again before closing and deleting.
				if updateChan, ok := md.observers[id]; ok {
					close(updateChan)
					delete(md.observers, id)
				}
			}()
		}
		return err
	})
	if err != nil {
		c = nil
	}

	return c, err
}

// getFoldersForRekey registers to receive updates about folders needing rekey actions.
func (md *MDServerRemote) getFoldersForRekey(ctx context.Context,
	client keybase1.MetadataClient, server *rpc.Server) error {
	// get this device's crypt public key
	cryptKey, err := md.config.KBPKI().GetCurrentCryptPublicKey(ctx)
	if err != nil {
		return err
	}
	// we'll get replies asynchronously as to not block the connection
	// for doing other active work for the user. they will be sent to
	// the FolderNeedsRekey handler.
	if err := server.Register(keybase1.MetadataUpdateProtocol(md)); err != nil {
		if _, ok := err.(rpc.AlreadyRegisteredError); !ok {
			return err
		}
	}
	return client.GetFoldersForRekey(ctx, cryptKey.KID)
}

// Shutdown implements the MDServer interface for MDServerRemote.
func (md *MDServerRemote) Shutdown() {
	// close the connection
	md.conn.Shutdown()
	// cancel pending observers
	md.cancelObservers()
	// cancel the ping ticker
	md.resetPingTicker(0)
	// cancel the auth token ticker
	if md.authToken != nil {
		md.authToken.Shutdown()
	}
}

//
// The below methods support the MD server acting as the key server.
// This will be the case for v1 of KBFS but we may move to our own
// separate key server at some point.
//

// GetTLFCryptKeyServerHalf is an implementation of the KeyServer interface.
func (md *MDServerRemote) GetTLFCryptKeyServerHalf(ctx context.Context,
	serverHalfID TLFCryptKeyServerHalfID) (TLFCryptKeyServerHalf, error) {
	// encode the ID
	idBytes, err := md.config.Codec().Encode(serverHalfID)
	if err != nil {
		return TLFCryptKeyServerHalf{}, err
	}
	// get the crypt public key
	cryptKey, err := md.config.KBPKI().GetCurrentCryptPublicKey(ctx)
	if err != nil {
		return TLFCryptKeyServerHalf{}, err
	}

	// get the key
	arg := keybase1.GetKeyArg{
		KeyHalfID: idBytes,
		DeviceKID: cryptKey.KID.String(),
		LogTags:   LogTagsFromContextToMap(ctx),
	}
	keyBytes, err := md.client.GetKey(ctx, arg)
	if err != nil {
		return TLFCryptKeyServerHalf{}, err
	}

	// decode the key
	var serverHalf TLFCryptKeyServerHalf
	err = md.config.Codec().Decode(keyBytes, &serverHalf)
	if err != nil {
		return TLFCryptKeyServerHalf{}, err
	}

	return serverHalf, nil
}

// PutTLFCryptKeyServerHalves is an implementation of the KeyServer interface.
func (md *MDServerRemote) PutTLFCryptKeyServerHalves(ctx context.Context,
	serverKeyHalves map[keybase1.UID]map[keybase1.KID]TLFCryptKeyServerHalf) error {
	// flatten out the map into an array
	var keyHalves []keybase1.KeyHalf
	for user, deviceMap := range serverKeyHalves {
		for deviceKID, serverHalf := range deviceMap {
			keyHalf, err := md.config.Codec().Encode(serverHalf)
			if err != nil {
				return err
			}
			keyHalves = append(keyHalves,
				keybase1.KeyHalf{
					User:      user,
					DeviceKID: deviceKID,
					Key:       keyHalf,
				})
		}
	}
	// put the keys
	arg := keybase1.PutKeysArg{
		KeyHalves: keyHalves,
		LogTags:   LogTagsFromContextToMap(ctx),
	}
	return md.client.PutKeys(ctx, arg)
}
