package libp2p

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TerraDharitri/drt-go-chain-core/core"
	"github.com/TerraDharitri/drt-go-chain-core/core/check"
	"github.com/TerraDharitri/drt-go-chain-core/core/throttler"
	commonCrypto "github.com/TerraDharitri/drt-go-chain-crypto"
	logger "github.com/TerraDharitri/drt-go-chain-logger"
	p2p "github.com/TerraDharitri/drt-go-chain-p2p"
	"github.com/TerraDharitri/drt-go-chain-p2p/config"
	"github.com/TerraDharitri/drt-go-chain-p2p/data"
	"github.com/TerraDharitri/drt-go-chain-p2p/debug"
	"github.com/TerraDharitri/drt-go-chain-p2p/libp2p/connectionMonitor"
	"github.com/TerraDharitri/drt-go-chain-p2p/libp2p/crypto"
	"github.com/TerraDharitri/drt-go-chain-p2p/libp2p/disabled"
	discoveryFactory "github.com/TerraDharitri/drt-go-chain-p2p/libp2p/discovery/factory"
	"github.com/TerraDharitri/drt-go-chain-p2p/libp2p/metrics"
	metricsFactory "github.com/TerraDharitri/drt-go-chain-p2p/libp2p/metrics/factory"
	"github.com/TerraDharitri/drt-go-chain-p2p/libp2p/networksharding/factory"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsubPb "github.com/libp2p/go-libp2p-pubsub/pb"
	libp2pCrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ws "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	webtransport "github.com/libp2p/go-libp2p/p2p/transport/webtransport"
)

const (
	// DirectSendID represents the protocol ID for sending and receiving direct P2P messages
	DirectSendID = protocol.ID("/drt/directsend/1.0.0")

	durationBetweenSends            = time.Microsecond * 10
	durationCheckConnections        = time.Second
	refreshPeersOnTopic             = time.Second * 3
	ttlPeersOnTopic                 = time.Second * 10
	ttlConnectionsWatcher           = time.Hour * 2
	pubsubTimeCacheDuration         = 10 * time.Minute
	acceptMessagesInAdvanceDuration = 20 * time.Second // we are accepting the messages with timestamp in the future only for this delta
	pollWaitForConnectionsInterval  = time.Second
	broadcastGoRoutines             = 1000
	timeBetweenPeerPrints           = time.Second * 20
	timeBetweenExternalLoggersCheck = time.Second * 20
	minRangePortValue               = 1025
	noSignPolicy                    = pubsub.MessageSignaturePolicy(0) // should be used only in tests
	msgBindError                    = "address already in use"
	maxRetriesIfBindError           = 10

	baseErrorSuffix      = "when creating a new network messenger"
	pubSubMaxMessageSize = 1 << 21 // 2 MB
)

type messageSigningConfig bool

const (
	withMessageSigning    messageSigningConfig = true
	withoutMessageSigning messageSigningConfig = false
)

// TODO remove the header size of the message when commit d3c5ecd3a3e884206129d9f2a9a4ddfd5e7c8951 from
// https://github.com/libp2p/go-libp2p-pubsub/pull/189/commits will be part of a new release
var messageHeader = 64 * 1024 // 64kB
var maxSendBuffSize = (1 << 21) - messageHeader
var log = logger.GetOrCreate("p2p/libp2p")

var _ p2p.Messenger = (*networkMessenger)(nil)
var externalPackages = []string{"dht", "nat", "basichost", "pubsub"}

func init() {
	pubsub.TimeCacheDuration = pubsubTimeCacheDuration

	for _, external := range externalPackages {
		_ = logger.GetOrCreate(fmt.Sprintf("external/%s", external))
	}
}

// TODO refactor this struct to have be a wrapper (with logic) over a glue code
type networkMessenger struct {
	p2pSigner
	ctx        context.Context
	cancelFunc context.CancelFunc
	p2pHost    ConnectableHost
	port       int
	pb         *pubsub.PubSub
	ds         p2p.DirectSender
	// TODO refactor this (connMonitor & connMonitorWrapper)
	connMonitor             ConnectionMonitor
	connMonitorWrapper      p2p.ConnectionMonitorWrapper
	peerDiscoverer          p2p.PeerDiscoverer
	sharder                 p2p.Sharder
	peerShardResolver       p2p.PeerShardResolver
	mutPeerResolver         sync.RWMutex
	mutTopics               sync.RWMutex
	processors              map[string]*topicProcessors
	topics                  map[string]*pubsub.Topic
	subscriptions           map[string]*pubsub.Subscription
	outgoingPLB             ChannelLoadBalancer
	poc                     *peersOnChannel
	goRoutinesThrottler     *throttler.NumGoRoutinesThrottler
	connectionsMetric       *metrics.Connections
	debugger                p2p.Debugger
	marshalizer             p2p.Marshalizer
	syncTimer               p2p.SyncTimer
	preferredPeersHolder    p2p.PreferredPeersHolderHandler
	printConnectionsWatcher p2p.ConnectionsWatcher
	peersRatingHandler      p2p.PeersRatingHandler
	mutPeerTopicNotifiers   sync.RWMutex
	peerTopicNotifiers      []p2p.PeerTopicNotifier
}

// ArgsNetworkMessenger defines the options used to create a p2p wrapper
type ArgsNetworkMessenger struct {
	Marshalizer           p2p.Marshalizer
	P2pConfig             config.P2PConfig
	SyncTimer             p2p.SyncTimer
	PreferredPeersHolder  p2p.PreferredPeersHolderHandler
	NodeOperationMode     p2p.NodeOperation
	PeersRatingHandler    p2p.PeersRatingHandler
	ConnectionWatcherType string
	P2pPrivateKey         commonCrypto.PrivateKey
	P2pSingleSigner       commonCrypto.SingleSigner
	P2pKeyGenerator       commonCrypto.KeyGenerator
}

// NewNetworkMessenger creates a libP2P messenger by opening a port on the current machine
func NewNetworkMessenger(args ArgsNetworkMessenger) (*networkMessenger, error) {
	return newNetworkMessenger(args, withMessageSigning)
}

func newNetworkMessenger(args ArgsNetworkMessenger, messageSigning messageSigningConfig) (*networkMessenger, error) {
	if check.IfNil(args.Marshalizer) {
		return nil, fmt.Errorf("%w %s", p2p.ErrNilMarshalizer, baseErrorSuffix)
	}
	if check.IfNil(args.SyncTimer) {
		return nil, fmt.Errorf("%w %s", p2p.ErrNilSyncTimer, baseErrorSuffix)
	}
	if check.IfNil(args.PreferredPeersHolder) {
		return nil, fmt.Errorf("%w %s", p2p.ErrNilPreferredPeersHolder, baseErrorSuffix)
	}
	if check.IfNil(args.PeersRatingHandler) {
		return nil, fmt.Errorf("%w %s", p2p.ErrNilPeersRatingHandler, baseErrorSuffix)
	}
	if check.IfNil(args.P2pPrivateKey) {
		return nil, fmt.Errorf("%w %s", p2p.ErrNilP2pPrivateKey, baseErrorSuffix)
	}
	if check.IfNil(args.P2pSingleSigner) {
		return nil, fmt.Errorf("%w %s", p2p.ErrNilP2pSingleSigner, baseErrorSuffix)
	}
	if check.IfNil(args.P2pKeyGenerator) {
		return nil, fmt.Errorf("%w %s", p2p.ErrNilP2pKeyGenerator, baseErrorSuffix)
	}

	setupExternalP2PLoggers()

	p2pNode, err := constructNodeWithPortRetry(args)
	if err != nil {
		return nil, err
	}

	err = addComponentsToNode(args, p2pNode, messageSigning)
	if err != nil {
		log.LogIfError(p2pNode.p2pHost.Close())
		return nil, err
	}

	return p2pNode, nil
}

func constructNode(
	args ArgsNetworkMessenger,
) (*networkMessenger, error) {

	port, err := getPort(args.P2pConfig.Node.Port, checkFreePort)
	if err != nil {
		return nil, err
	}

	log.Debug("connectionWatcherType", "type", args.ConnectionWatcherType)
	connWatcher, err := metricsFactory.NewConnectionsWatcher(args.ConnectionWatcherType, ttlConnectionsWatcher)
	if err != nil {
		return nil, err
	}

	p2pPrivateKey, err := crypto.ConvertPrivateKeyToLibp2pPrivateKey(args.P2pPrivateKey)
	if err != nil {
		return nil, err
	}

	transportOptions, addresses, err := parseTransportOptions(args.P2pConfig.Node.Transports, port)
	if err != nil {
		return nil, err
	}

	options := []libp2p.Option{
		libp2p.ListenAddrStrings(addresses...),
		libp2p.Identity(p2pPrivateKey),
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
		// we need to disable relay option in order to save the node's bandwidth as much as possible
		libp2p.DisableRelay(),
		libp2p.NATPortMap(),
	}
	options = append(options, transportOptions...)

	h, err := libp2p.New(options...)
	if err != nil {
		return nil, err
	}

	p2pSignerArgs := crypto.ArgsP2pSignerWrapper{
		PrivateKey: args.P2pPrivateKey,
		Signer:     args.P2pSingleSigner,
		KeyGen:     args.P2pKeyGenerator,
	}

	p2pSignerInstance, err := crypto.NewP2PSignerWrapper(p2pSignerArgs)
	if err != nil {
		return nil, err
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	p2pNode := &networkMessenger{
		p2pSigner:               p2pSignerInstance,
		ctx:                     ctx,
		cancelFunc:              cancelFunc,
		p2pHost:                 NewConnectableHost(h),
		port:                    port,
		printConnectionsWatcher: connWatcher,
		peersRatingHandler:      args.PeersRatingHandler,
		peerTopicNotifiers:      make([]p2p.PeerTopicNotifier, 0),
	}

	return p2pNode, nil
}

func parseTransportOptions(configs config.TransportConfig, port int) ([]libp2p.Option, []string, error) {
	options := make([]libp2p.Option, 0)
	addresses := make([]string, 0)

	tcpAddress := configs.TCP.ListenAddress
	if len(tcpAddress) > 0 {
		if !strictCheckStringForIntMarkup(tcpAddress) {
			return nil, nil, p2p.ErrInvalidTCPAddress
		}

		addresses = append(addresses, fmt.Sprintf(tcpAddress, port))
		if configs.TCP.PreventPortReuse {
			options = append(options, libp2p.Transport(tcp.NewTCPTransport, tcp.DisableReuseport()))
		} else {
			options = append(options, libp2p.Transport(tcp.NewTCPTransport))
		}
	}

	quicAddress := configs.QUICAddress
	if len(quicAddress) > 0 {
		if !strictCheckStringForIntMarkup(quicAddress) {
			return nil, nil, p2p.ErrInvalidQUICAddress
		}

		addresses = append(addresses, fmt.Sprintf(quicAddress, port))
		options = append(options, libp2p.Transport(quic.NewTransport))
	}

	webSocketAddress := configs.WebSocketAddress
	if len(webSocketAddress) > 0 {
		if !strictCheckStringForIntMarkup(webSocketAddress) {
			return nil, nil, p2p.ErrInvalidWSAddress
		}

		addresses = append(addresses, fmt.Sprintf(webSocketAddress, port))
		options = append(options, libp2p.Transport(ws.New))
	}

	webTransportAddress := configs.WebTransportAddress
	if len(webTransportAddress) > 0 {
		if !strictCheckStringForIntMarkup(webTransportAddress) {
			return nil, nil, p2p.ErrInvalidWebTransportAddress
		}

		addresses = append(addresses, fmt.Sprintf(webTransportAddress, port))
		options = append(options, libp2p.Transport(webtransport.New))
	}

	if len(addresses) == 0 {
		return nil, nil, p2p.ErrNoTransportsDefined
	}

	return options, addresses, nil
}

func strictCheckStringForIntMarkup(str string) bool {
	intMarkup := "%d"
	return strings.Count(str, intMarkup) == 1
}

func constructNodeWithPortRetry(
	args ArgsNetworkMessenger,
) (*networkMessenger, error) {

	var lastErr error
	for i := 0; i < maxRetriesIfBindError; i++ {
		p2pNode, err := constructNode(args)
		if err == nil {
			return p2pNode, nil
		}

		lastErr = err
		if !strings.Contains(err.Error(), msgBindError) {
			// not a bind error, return directly
			return nil, err
		}

		log.Debug("bind error in network messenger", "retry number", i+1, "error", err)
	}

	return nil, lastErr
}

func setupExternalP2PLoggers() {
	_ = logging.SetLogLevel("*", "PANIC")

	for _, external := range externalPackages {
		logLevel := logger.GetLoggerLogLevel("external/" + external)
		if logLevel > logger.LogTrace {
			continue
		}

		_ = logging.SetLogLevel(external, "DEBUG")
	}
}

func addComponentsToNode(
	args ArgsNetworkMessenger,
	p2pNode *networkMessenger,
	messageSigning messageSigningConfig,
) error {
	var err error

	p2pNode.processors = make(map[string]*topicProcessors)
	p2pNode.topics = make(map[string]*pubsub.Topic)
	p2pNode.subscriptions = make(map[string]*pubsub.Subscription)
	p2pNode.outgoingPLB = NewOutgoingChannelLoadBalancer()
	p2pNode.peerShardResolver = &unknownPeerShardResolver{}
	p2pNode.marshalizer = args.Marshalizer
	p2pNode.syncTimer = args.SyncTimer
	p2pNode.preferredPeersHolder = args.PreferredPeersHolder
	p2pNode.debugger = debug.NewP2PDebugger(core.PeerID(p2pNode.p2pHost.ID()))
	p2pNode.peersRatingHandler = args.PeersRatingHandler

	err = p2pNode.createPubSub(messageSigning)
	if err != nil {
		return err
	}

	err = p2pNode.createSharder(args)
	if err != nil {
		return err
	}

	err = p2pNode.createDiscoverer(args.P2pConfig)
	if err != nil {
		return err
	}

	err = p2pNode.createConnectionMonitor(args.P2pConfig)
	if err != nil {
		return err
	}

	p2pNode.createConnectionsMetric()

	p2pNode.ds, err = NewDirectSender(p2pNode.ctx, p2pNode.p2pHost, p2pNode.directMessageHandler, p2pNode)
	if err != nil {
		return err
	}

	p2pNode.goRoutinesThrottler, err = throttler.NewNumGoRoutinesThrottler(broadcastGoRoutines)
	if err != nil {
		return err
	}

	p2pNode.printLogs()

	return nil
}

func (netMes *networkMessenger) createPubSub(messageSigning messageSigningConfig) error {
	optsPS := make([]pubsub.Option, 0)
	if messageSigning == withoutMessageSigning {
		log.Warn("signature verification is turned off in network messenger instance. NOT recommended in production environment")
		optsPS = append(optsPS, pubsub.WithMessageSignaturePolicy(noSignPolicy))
	}

	optsPS = append(optsPS,
		pubsub.WithPeerFilter(netMes.newPeerFound),
		pubsub.WithMaxMessageSize(pubSubMaxMessageSize),
	)

	var err error
	netMes.pb, err = pubsub.NewGossipSub(netMes.ctx, netMes.p2pHost, optsPS...)
	if err != nil {
		return err
	}

	netMes.poc, err = newPeersOnChannel(
		netMes.peersRatingHandler,
		netMes.pb.ListPeers,
		refreshPeersOnTopic,
		ttlPeersOnTopic)
	if err != nil {
		return err
	}

	go func(plb ChannelLoadBalancer) {
		for {
			select {
			case <-time.After(durationBetweenSends):
			case <-netMes.ctx.Done():
				log.Debug("closing networkMessenger's send from channel load balancer go routine")
				return
			}

			sendableData := plb.CollectOneElementFromChannels()
			if sendableData == nil {
				continue
			}

			netMes.mutTopics.RLock()
			topic := netMes.topics[sendableData.Topic]
			netMes.mutTopics.RUnlock()

			if topic == nil {
				log.Warn("writing on a topic that the node did not register on - message dropped",
					"topic", sendableData.Topic,
				)

				continue
			}

			packedSendableDataBuff := netMes.createMessageBytes(sendableData.Buff)
			if len(packedSendableDataBuff) == 0 {
				continue
			}

			errPublish := netMes.publish(topic, sendableData, packedSendableDataBuff)
			if errPublish != nil {
				log.Trace("error sending data", "error", errPublish)
			}
		}
	}(netMes.outgoingPLB)

	return nil
}

func (netMes *networkMessenger) newPeerFound(pid peer.ID, topic string) bool {
	netMes.mutPeerTopicNotifiers.RLock()
	defer netMes.mutPeerTopicNotifiers.RUnlock()
	for _, notifier := range netMes.peerTopicNotifiers {
		notifier.NewPeerFound(core.PeerID(pid), topic)
	}

	return true
}

func (netMes *networkMessenger) publish(topic *pubsub.Topic, data *SendableData, packedSendableDataBuff []byte) error {
	options := make([]pubsub.PubOpt, 0, 1)

	if data.Sk != nil {
		options = append(options, pubsub.WithSecretKeyAndPeerId(data.Sk, data.ID))
	}

	return topic.Publish(netMes.ctx, packedSendableDataBuff, options...)
}

func (netMes *networkMessenger) createMessageBytes(buff []byte) []byte {
	message := &data.TopicMessage{
		Version:   currentTopicMessageVersion,
		Payload:   buff,
		Timestamp: netMes.syncTimer.CurrentTime().Unix(),
	}

	buffToSend, errMarshal := netMes.marshalizer.Marshal(message)
	if errMarshal != nil {
		log.Warn("error sending data", "error", errMarshal)
		return nil
	}

	return buffToSend
}

func (netMes *networkMessenger) createSharder(argsNetMes ArgsNetworkMessenger) error {
	args := factory.ArgsSharderFactory{
		PeerShardResolver:    &unknownPeerShardResolver{},
		Pid:                  netMes.p2pHost.ID(),
		P2pConfig:            argsNetMes.P2pConfig,
		PreferredPeersHolder: netMes.preferredPeersHolder,
		NodeOperationMode:    argsNetMes.NodeOperationMode,
	}

	var err error
	netMes.sharder, err = factory.NewSharder(args)

	return err
}

func (netMes *networkMessenger) createDiscoverer(p2pConfig config.P2PConfig) error {
	var err error

	args := discoveryFactory.ArgsPeerDiscoverer{
		Context:            netMes.ctx,
		Host:               netMes.p2pHost,
		Sharder:            netMes.sharder,
		P2pConfig:          p2pConfig,
		ConnectionsWatcher: netMes.printConnectionsWatcher,
	}

	netMes.peerDiscoverer, err = discoveryFactory.NewPeerDiscoverer(args)

	return err
}

func (netMes *networkMessenger) createConnectionMonitor(p2pConfig config.P2PConfig) error {
	reconnecter, ok := netMes.peerDiscoverer.(p2p.Reconnecter)
	if !ok {
		return fmt.Errorf("%w when converting peerDiscoverer to reconnecter interface", p2p.ErrWrongTypeAssertion)
	}

	sharder, ok := netMes.sharder.(connectionMonitor.Sharder)
	if !ok {
		return fmt.Errorf("%w in networkMessenger.createConnectionMonitor", p2p.ErrWrongTypeAssertions)
	}

	args := connectionMonitor.ArgsConnectionMonitorSimple{
		Reconnecter:                reconnecter,
		Sharder:                    sharder,
		ThresholdMinConnectedPeers: p2pConfig.Node.ThresholdMinConnectedPeers,
		PreferredPeersHolder:       netMes.preferredPeersHolder,
		ConnectionsWatcher:         netMes.printConnectionsWatcher,
	}
	var err error
	netMes.connMonitor, err = connectionMonitor.NewLibp2pConnectionMonitorSimple(args)
	if err != nil {
		return err
	}

	cmw := newConnectionMonitorWrapper(
		netMes.p2pHost.Network(),
		netMes.connMonitor,
		&disabled.PeerDenialEvaluator{},
	)
	netMes.p2pHost.Network().Notify(cmw)
	netMes.connMonitorWrapper = cmw

	go func() {
		for {
			cmw.CheckConnectionsBlocking()
			select {
			case <-time.After(durationCheckConnections):
			case <-netMes.ctx.Done():
				log.Debug("peer monitoring go routine is stopping...")
				return
			}
		}
	}()

	return nil
}

func (netMes *networkMessenger) createConnectionsMetric() {
	netMes.connectionsMetric = metrics.NewConnections()
	netMes.p2pHost.Network().Notify(netMes.connectionsMetric)
}

func (netMes *networkMessenger) printLogs() {
	addresses := make([]interface{}, 0)
	for i, address := range netMes.p2pHost.Addrs() {
		addresses = append(addresses, fmt.Sprintf("addr%d", i))
		addresses = append(addresses, address.String()+"/p2p/"+netMes.ID().Pretty())
	}
	log.Info("listening on addresses", addresses...)

	go netMes.printLogsStats()
	go netMes.checkExternalLoggers()
}

func (netMes *networkMessenger) printLogsStats() {
	for {
		select {
		case <-netMes.ctx.Done():
			log.Debug("closing networkMessenger.printLogsStats go routine")
			return
		case <-time.After(timeBetweenPeerPrints):
		}

		conns := netMes.connectionsMetric.ResetNumConnections()
		disconns := netMes.connectionsMetric.ResetNumDisconnections()

		peersInfo := netMes.GetConnectedPeersInfo()
		log.Debug("network connection status",
			"known peers", len(netMes.Peers()),
			"connected peers", len(netMes.ConnectedPeers()),
			"intra shard validators", peersInfo.NumIntraShardValidators,
			"intra shard observers", peersInfo.NumIntraShardObservers,
			"cross shard validators", peersInfo.NumCrossShardValidators,
			"cross shard observers", peersInfo.NumCrossShardObservers,
			"full history observers", peersInfo.NumFullHistoryObservers,
			"unknown", len(peersInfo.UnknownPeers),
			"seeders", len(peersInfo.Seeders),
			"current shard", peersInfo.SelfShardID,
			"validators histogram", netMes.mapHistogram(peersInfo.NumValidatorsOnShard),
			"observers histogram", netMes.mapHistogram(peersInfo.NumObserversOnShard),
			"preferred peers histogram", netMes.mapHistogram(peersInfo.NumPreferredPeersOnShard),
		)

		connsPerSec := conns / uint32(timeBetweenPeerPrints/time.Second)
		disconnsPerSec := disconns / uint32(timeBetweenPeerPrints/time.Second)

		log.Debug("network connection metrics",
			"connections/s", connsPerSec,
			"disconnections/s", disconnsPerSec,
			"connections", conns,
			"disconnections", disconns,
			"time", timeBetweenPeerPrints,
		)
	}
}

func (netMes *networkMessenger) mapHistogram(input map[uint32]int) string {
	keys := make([]uint32, 0, len(input))
	for shard := range input {
		keys = append(keys, shard)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	vals := make([]string, 0, len(keys))
	for _, key := range keys {
		var shard string
		if key == core.MetachainShardId {
			shard = "meta"
		} else {
			shard = fmt.Sprintf("shard %d", key)
		}

		vals = append(vals, fmt.Sprintf("%s: %d", shard, input[key]))
	}

	return strings.Join(vals, ", ")
}

func (netMes *networkMessenger) checkExternalLoggers() {
	for {
		select {
		case <-netMes.ctx.Done():
			log.Debug("closing networkMessenger.checkExternalLoggers go routine")
			return
		case <-time.After(timeBetweenExternalLoggersCheck):
		}

		setupExternalP2PLoggers()
	}
}

// Close closes the host, connections and streams
func (netMes *networkMessenger) Close() error {
	log.Debug("closing network messenger's host...")

	var err error
	errHost := netMes.p2pHost.Close()
	if errHost != nil {
		err = errHost
		log.Warn("networkMessenger.Close",
			"component", "host",
			"error", err)
	}

	log.Debug("closing network messenger's print connection watcher...")
	errConnWatcher := netMes.printConnectionsWatcher.Close()
	if errConnWatcher != nil {
		err = errConnWatcher
		log.Warn("networkMessenger.Close",
			"component", "connectionsWatcher",
			"error", err)
	}

	log.Debug("closing network messenger's outgoing load balancer...")
	errOplb := netMes.outgoingPLB.Close()
	if errOplb != nil {
		err = errOplb
		log.Warn("networkMessenger.Close",
			"component", "outgoingPLB",
			"error", err)
	}

	log.Debug("closing network messenger's peers on channel...")
	errPoc := netMes.poc.Close()
	if errPoc != nil {
		log.Warn("networkMessenger.Close",
			"component", "peersOnChannel",
			"error", errPoc)
	}

	log.Debug("closing network messenger's connection monitor...")
	errConnMonitor := netMes.connMonitor.Close()
	if errConnMonitor != nil {
		log.Warn("networkMessenger.Close",
			"component", "connMonitor",
			"error", errConnMonitor)
	}

	log.Debug("closing network messenger's components through the context...")
	netMes.cancelFunc()

	log.Debug("closing network messenger's debugger...")
	errDebugger := netMes.debugger.Close()
	if errDebugger != nil {
		err = errDebugger
		log.Warn("networkMessenger.Close",
			"component", "debugger",
			"error", err)
	}

	log.Debug("closing network messenger's peerstore...")
	errPeerStore := netMes.p2pHost.Peerstore().Close()
	if errPeerStore != nil {
		err = errPeerStore
		log.Warn("networkMessenger.Close",
			"component", "peerstore",
			"error", err)
	}

	if err == nil {
		log.Info("network messenger closed successfully")
	}

	return err
}

// ID returns the messenger's ID
func (netMes *networkMessenger) ID() core.PeerID {
	h := netMes.p2pHost

	return core.PeerID(h.ID())
}

// Peers returns the list of all known peers ID (including self)
func (netMes *networkMessenger) Peers() []core.PeerID {
	peers := make([]core.PeerID, 0)

	for _, p := range netMes.p2pHost.Peerstore().Peers() {
		peers = append(peers, core.PeerID(p))
	}
	return peers
}

// Addresses returns all addresses found in peerstore
func (netMes *networkMessenger) Addresses() []string {
	addrs := make([]string, 0)

	for _, address := range netMes.p2pHost.Addrs() {
		addrs = append(addrs, address.String()+"/p2p/"+netMes.ID().Pretty())
	}

	return addrs
}

// ConnectToPeer tries to open a new connection to a peer
func (netMes *networkMessenger) ConnectToPeer(address string) error {
	return netMes.p2pHost.ConnectToPeer(netMes.ctx, address)
}

// Bootstrap will start the peer discovery mechanism
func (netMes *networkMessenger) Bootstrap() error {
	err := netMes.peerDiscoverer.Bootstrap()
	if err == nil {
		log.Info("started the network discovery process...")
	}
	return err
}

// WaitForConnections will wait the maxWaitingTime duration or until the target connected peers was achieved
func (netMes *networkMessenger) WaitForConnections(maxWaitingTime time.Duration, minNumOfPeers uint32) {
	startTime := time.Now()
	defer func() {
		log.Debug("networkMessenger.WaitForConnections",
			"waited", time.Since(startTime), "num connected peers", len(netMes.ConnectedPeers()))
	}()

	if minNumOfPeers == 0 {
		log.Debug("networkMessenger.WaitForConnections", "waiting", maxWaitingTime)
		time.Sleep(maxWaitingTime)
		return
	}

	netMes.waitForConnections(maxWaitingTime, minNumOfPeers)
}

func (netMes *networkMessenger) waitForConnections(maxWaitingTime time.Duration, minNumOfPeers uint32) {
	log.Debug("networkMessenger.WaitForConnections", "waiting", maxWaitingTime, "min num of peers", minNumOfPeers)
	ctxMaxWaitingTime, cancel := context.WithTimeout(context.Background(), maxWaitingTime)
	defer cancel()

	for {
		if netMes.shouldStopWaiting(ctxMaxWaitingTime, minNumOfPeers) {
			return
		}
	}
}

func (netMes *networkMessenger) shouldStopWaiting(ctxMaxWaitingTime context.Context, minNumOfPeers uint32) bool {
	ctx, cancel := context.WithTimeout(context.Background(), pollWaitForConnectionsInterval)
	defer cancel()

	select {
	case <-ctxMaxWaitingTime.Done():
		return true
	case <-ctx.Done():
		return int(minNumOfPeers) <= len(netMes.ConnectedPeers())
	}
}

// IsConnected returns true if current node is connected to provided peer
func (netMes *networkMessenger) IsConnected(peerID core.PeerID) bool {
	h := netMes.p2pHost

	connectedness := h.Network().Connectedness(peer.ID(peerID))

	return connectedness == network.Connected
}

// ConnectedPeers returns the current connected peers list
func (netMes *networkMessenger) ConnectedPeers() []core.PeerID {
	h := netMes.p2pHost

	connectedPeers := make(map[core.PeerID]struct{})

	for _, conn := range h.Network().Conns() {
		p := core.PeerID(conn.RemotePeer())

		if netMes.IsConnected(p) {
			connectedPeers[p] = struct{}{}
		}
	}

	peerList := make([]core.PeerID, len(connectedPeers))

	index := 0
	for k := range connectedPeers {
		peerList[index] = k
		index++
	}

	return peerList
}

// ConnectedAddresses returns all connected peer's addresses
func (netMes *networkMessenger) ConnectedAddresses() []string {
	h := netMes.p2pHost
	conns := make([]string, 0)

	for _, c := range h.Network().Conns() {
		conns = append(conns, c.RemoteMultiaddr().String()+"/p2p/"+c.RemotePeer().String())
	}
	return conns
}

// PeerAddresses returns the peer's addresses or empty slice if the peer is unknown
func (netMes *networkMessenger) PeerAddresses(pid core.PeerID) []string {
	h := netMes.p2pHost
	result := make([]string, 0)

	// check if the peer is connected to return it's connected address
	for _, c := range h.Network().Conns() {
		if string(c.RemotePeer()) == string(pid.Bytes()) {
			result = append(result, c.RemoteMultiaddr().String())
			break
		}
	}

	// check in peerstore (maybe it is known but not connected)
	addresses := h.Peerstore().Addrs(peer.ID(pid.Bytes()))
	for _, addr := range addresses {
		result = append(result, addr.String())
	}

	return result
}

// ConnectedPeersOnTopic returns the connected peers on a provided topic
func (netMes *networkMessenger) ConnectedPeersOnTopic(topic string) []core.PeerID {
	return netMes.poc.ConnectedPeersOnChannel(topic)
}

// ConnectedFullHistoryPeersOnTopic returns the connected peers on a provided topic
func (netMes *networkMessenger) ConnectedFullHistoryPeersOnTopic(topic string) []core.PeerID {
	peerList := netMes.ConnectedPeersOnTopic(topic)
	fullHistoryList := make([]core.PeerID, 0)
	for _, topicPeer := range peerList {
		peerInfo := netMes.peerShardResolver.GetPeerInfo(topicPeer)
		if peerInfo.PeerSubType == core.FullHistoryObserver {
			fullHistoryList = append(fullHistoryList, topicPeer)
		}
	}

	return fullHistoryList
}

// CreateTopic opens a new topic using pubsub infrastructure
func (netMes *networkMessenger) CreateTopic(name string, createChannelForTopic bool) error {
	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()
	_, found := netMes.topics[name]
	if found {
		return nil
	}

	topic, err := netMes.pb.Join(name)
	if err != nil {
		return fmt.Errorf("%w for topic %s", err, name)
	}

	netMes.topics[name] = topic
	subscrRequest, err := topic.Subscribe()
	if err != nil {
		return fmt.Errorf("%w for topic %s", err, name)
	}

	netMes.subscriptions[name] = subscrRequest
	if createChannelForTopic {
		err = netMes.outgoingPLB.AddChannel(name)
	}

	// just a dummy func to consume messages received by the newly created topic
	go func() {
		var errSubscrNext error
		for {
			_, errSubscrNext = subscrRequest.Next(netMes.ctx)
			if errSubscrNext != nil {
				log.Debug("closed subscription",
					"topic", subscrRequest.Topic(),
					"err", errSubscrNext,
				)
				return
			}
		}
	}()

	return err
}

// HasTopic returns true if the topic has been created
func (netMes *networkMessenger) HasTopic(name string) bool {
	netMes.mutTopics.RLock()
	_, found := netMes.topics[name]
	netMes.mutTopics.RUnlock()

	return found
}

// BroadcastOnChannelBlocking tries to send a byte buffer onto a topic using provided channel
// It is a blocking method. It needs to be launched on a go routine
func (netMes *networkMessenger) BroadcastOnChannelBlocking(channel string, topic string, buff []byte) error {
	err := netMes.checkSendableData(buff)
	if err != nil {
		return err
	}

	if !netMes.goRoutinesThrottler.CanProcess() {
		return p2p.ErrTooManyGoroutines
	}

	netMes.goRoutinesThrottler.StartProcessing()

	sendable := &SendableData{
		Buff:  buff,
		Topic: topic,
		ID:    netMes.p2pHost.ID(),
	}
	netMes.outgoingPLB.GetChannelOrDefault(channel) <- sendable
	netMes.goRoutinesThrottler.EndProcessing()
	return nil
}

func (netMes *networkMessenger) checkSendableData(buff []byte) error {
	if len(buff) > maxSendBuffSize {
		return fmt.Errorf("%w, to be sent: %d, maximum: %d", p2p.ErrMessageTooLarge, len(buff), maxSendBuffSize)
	}
	if len(buff) == 0 {
		return p2p.ErrEmptyBufferToSend
	}

	return nil
}

// BroadcastOnChannel tries to send a byte buffer onto a topic using provided channel
func (netMes *networkMessenger) BroadcastOnChannel(channel string, topic string, buff []byte) {
	go func() {
		err := netMes.BroadcastOnChannelBlocking(channel, topic, buff)
		if err != nil {
			log.Warn("p2p broadcast", "error", err.Error())
		}
	}()
}

// Broadcast tries to send a byte buffer onto a topic using the topic name as channel
func (netMes *networkMessenger) Broadcast(topic string, buff []byte) {
	netMes.BroadcastOnChannel(topic, topic, buff)
}

// BroadcastOnChannelBlockingUsingPrivateKey tries to send a byte buffer onto a topic using provided channel
// It is a blocking method. It needs to be launched on a go routine
func (netMes *networkMessenger) BroadcastOnChannelBlockingUsingPrivateKey(
	channel string,
	topic string,
	buff []byte,
	pid core.PeerID,
	skBytes []byte,
) error {
	id := peer.ID(pid)
	sk, err := libp2pCrypto.UnmarshalSecp256k1PrivateKey(skBytes)
	if err != nil {
		return err
	}

	err = netMes.checkSendableData(buff)
	if err != nil {
		return err
	}

	if !netMes.goRoutinesThrottler.CanProcess() {
		return p2p.ErrTooManyGoroutines
	}

	netMes.goRoutinesThrottler.StartProcessing()

	sendable := &SendableData{
		Buff:  buff,
		Topic: topic,
		Sk:    sk,
		ID:    id,
	}
	netMes.outgoingPLB.GetChannelOrDefault(channel) <- sendable
	netMes.goRoutinesThrottler.EndProcessing()
	return nil
}

// BroadcastOnChannelUsingPrivateKey tries to send a byte buffer onto a topic using provided channel
func (netMes *networkMessenger) BroadcastOnChannelUsingPrivateKey(
	channel string,
	topic string,
	buff []byte,
	pid core.PeerID,
	skBytes []byte,
) {
	go func() {
		err := netMes.BroadcastOnChannelBlockingUsingPrivateKey(channel, topic, buff, pid, skBytes)
		if err != nil {
			log.Warn("p2p broadcast using private key", "error", err.Error())
		}
	}()
}

// BroadcastUsingPrivateKey tries to send a byte buffer onto a topic using the topic name as channel
func (netMes *networkMessenger) BroadcastUsingPrivateKey(
	topic string,
	buff []byte,
	pid core.PeerID,
	skBytes []byte,
) {
	netMes.BroadcastOnChannelUsingPrivateKey(topic, topic, buff, pid, skBytes)
}

// RegisterMessageProcessor registers a message process on a topic. The function allows registering multiple handlers
// on a topic. Each handler should be associated with a new identifier on the same topic. Using same identifier on different
// topics is allowed. The order of handler calling on a particular topic is not deterministic.
func (netMes *networkMessenger) RegisterMessageProcessor(topic string, identifier string, handler p2p.MessageProcessor) error {
	if check.IfNil(handler) {
		return fmt.Errorf("%w when calling networkMessenger.RegisterMessageProcessor for topic %s",
			p2p.ErrNilValidator, topic)
	}

	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()
	topicProcs := netMes.processors[topic]
	if topicProcs == nil {
		topicProcs = newTopicProcessors()
		netMes.processors[topic] = topicProcs

		err := netMes.pb.RegisterTopicValidator(topic, netMes.pubsubCallback(topicProcs, topic))
		if err != nil {
			return err
		}
	}

	err := topicProcs.addTopicProcessor(identifier, handler)
	if err != nil {
		return fmt.Errorf("%w, topic %s", err, topic)
	}

	return nil
}

func (netMes *networkMessenger) pubsubCallback(topicProcs *topicProcessors, topic string) func(ctx context.Context, pid peer.ID, message *pubsub.Message) bool {
	return func(ctx context.Context, pid peer.ID, message *pubsub.Message) bool {
		fromConnectedPeer := core.PeerID(pid)
		msg, err := netMes.transformAndCheckMessage(message, fromConnectedPeer, topic)
		if err != nil {
			log.Trace("p2p validator - new message", "error", err.Error(), "topic", topic)
			return false
		}

		identifiers, handlers := topicProcs.getList()
		messageOk := true
		for index, handler := range handlers {
			err = handler.ProcessReceivedMessage(msg, fromConnectedPeer)
			if err != nil {
				log.Trace("p2p validator",
					"error", err.Error(),
					"topic", topic,
					"originator", p2p.MessageOriginatorPid(msg),
					"from connected peer", p2p.PeerIdToShortString(fromConnectedPeer),
					"seq no", p2p.MessageOriginatorSeq(msg),
					"topic identifier", identifiers[index],
				)
				messageOk = false
			}
		}
		netMes.processDebugMessage(topic, fromConnectedPeer, uint64(len(message.Data)), !messageOk)

		if messageOk {
			netMes.peersRatingHandler.IncreaseRating(fromConnectedPeer)
		}

		return messageOk
	}
}

func (netMes *networkMessenger) transformAndCheckMessage(pbMsg *pubsub.Message, pid core.PeerID, topic string) (p2p.MessageP2P, error) {
	msg, errUnmarshal := NewMessage(pbMsg, netMes.marshalizer)
	if errUnmarshal != nil {
		// this error is so severe that will need to blacklist both the originator and the connected peer as there is
		// no way this node can communicate with them
		pidFrom := core.PeerID(pbMsg.From)
		netMes.blacklistPid(pid, p2p.WrongP2PMessageBlacklistDuration)
		netMes.blacklistPid(pidFrom, p2p.WrongP2PMessageBlacklistDuration)

		return nil, errUnmarshal
	}

	err := netMes.validMessageByTimestamp(msg)
	if err != nil {
		// not reprocessing nor re-broadcasting the same message over and over again
		log.Trace("received an invalid message",
			"originator pid", p2p.MessageOriginatorPid(msg),
			"from connected pid", p2p.PeerIdToShortString(pid),
			"sequence", hex.EncodeToString(msg.SeqNo()),
			"timestamp", msg.Timestamp(),
			"error", err,
		)
		netMes.processDebugMessage(topic, pid, uint64(len(msg.Data())), true)

		return nil, err
	}

	return msg, nil
}

func (netMes *networkMessenger) blacklistPid(pid core.PeerID, banDuration time.Duration) {
	if netMes.connMonitorWrapper.PeerDenialEvaluator().IsDenied(pid) {
		return
	}
	if len(pid) == 0 {
		return
	}

	log.Debug("blacklisted due to incompatible p2p message",
		"pid", pid.Pretty(),
		"time", banDuration,
	)

	err := netMes.connMonitorWrapper.PeerDenialEvaluator().UpsertPeerID(pid, banDuration)
	if err != nil {
		log.Warn("error blacklisting peer ID in network messnger",
			"pid", pid.Pretty(),
			"error", err.Error(),
		)
	}
}

// invalidMessageByTimestamp will check that the message time stamp should be in the interval
// (now-pubsubTimeCacheDuration+acceptMessagesInAdvanceDuration, now+acceptMessagesInAdvanceDuration)
func (netMes *networkMessenger) validMessageByTimestamp(msg p2p.MessageP2P) error {
	now := netMes.syncTimer.CurrentTime()
	isInFuture := now.Add(acceptMessagesInAdvanceDuration).Unix() < msg.Timestamp()
	if isInFuture {
		return fmt.Errorf("%w, self timestamp %d, message timestamp %d",
			p2p.ErrMessageTooNew, now.Unix(), msg.Timestamp())
	}

	past := now.Unix() - int64(pubsubTimeCacheDuration.Seconds())
	if msg.Timestamp() < past {
		return fmt.Errorf("%w, self timestamp %d, message timestamp %d",
			p2p.ErrMessageTooOld, now.Unix(), msg.Timestamp())
	}

	return nil
}

func (netMes *networkMessenger) processDebugMessage(topic string, fromConnectedPeer core.PeerID, size uint64, isRejected bool) {
	if fromConnectedPeer == netMes.ID() {
		netMes.debugger.AddOutgoingMessage(topic, size, isRejected)
	} else {
		netMes.debugger.AddIncomingMessage(topic, size, isRejected)
	}
}

// UnregisterAllMessageProcessors will unregister all message processors for topics
func (netMes *networkMessenger) UnregisterAllMessageProcessors() error {
	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()

	for topic := range netMes.processors {
		err := netMes.pb.UnregisterTopicValidator(topic)
		if err != nil {
			return err
		}

		delete(netMes.processors, topic)
	}
	return nil
}

// UnjoinAllTopics call close on all topics
func (netMes *networkMessenger) UnjoinAllTopics() error {
	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()

	var errFound error
	for topicName, t := range netMes.topics {
		subscr := netMes.subscriptions[topicName]
		if subscr != nil {
			subscr.Cancel()
		}

		err := t.Close()
		if err != nil {
			log.Warn("error closing topic",
				"topic", topicName,
				"error", err,
			)
			errFound = err
		}

		delete(netMes.topics, topicName)
	}

	return errFound
}

// UnregisterMessageProcessor unregisters a message processes on a topic
func (netMes *networkMessenger) UnregisterMessageProcessor(topic string, identifier string) error {
	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()

	topicProcs := netMes.processors[topic]
	if topicProcs == nil {
		return nil
	}

	err := topicProcs.removeTopicProcessor(identifier)
	if err != nil {
		return err
	}

	identifiers, _ := topicProcs.getList()
	if len(identifiers) == 0 {
		netMes.processors[topic] = nil

		return netMes.pb.UnregisterTopicValidator(topic)
	}

	return nil
}

// SendToConnectedPeer sends a direct message to a connected peer
func (netMes *networkMessenger) SendToConnectedPeer(topic string, buff []byte, peerID core.PeerID) error {
	err := netMes.checkSendableData(buff)
	if err != nil {
		return err
	}

	buffToSend := netMes.createMessageBytes(buff)
	if len(buffToSend) == 0 {
		return nil
	}

	if peerID == netMes.ID() {
		return netMes.sendDirectToSelf(topic, buffToSend)
	}

	err = netMes.ds.Send(topic, buffToSend, peerID)
	netMes.debugger.AddOutgoingMessage(topic, uint64(len(buffToSend)), err != nil)

	return err
}

func (netMes *networkMessenger) sendDirectToSelf(topic string, buff []byte) error {
	msg := &pubsub.Message{
		Message: &pubsubPb.Message{
			From:      netMes.ID().Bytes(),
			Data:      buff,
			Seqno:     netMes.ds.NextSequenceNumber(),
			Topic:     &topic,
			Signature: netMes.ID().Bytes(),
		},
	}

	return netMes.directMessageHandler(msg, netMes.ID())
}

func (netMes *networkMessenger) directMessageHandler(message *pubsub.Message, fromConnectedPeer core.PeerID) error {
	topic := *message.Topic
	msg, err := netMes.transformAndCheckMessage(message, fromConnectedPeer, topic)
	if err != nil {
		return err
	}

	netMes.mutTopics.RLock()
	topicProcs := netMes.processors[topic]
	netMes.mutTopics.RUnlock()

	if topicProcs == nil {
		return fmt.Errorf("%w on directMessageHandler for topic %s", p2p.ErrNilValidator, topic)
	}
	identifiers, handlers := topicProcs.getList()

	go func(msg p2p.MessageP2P) {
		if check.IfNil(msg) {
			return
		}

		// we won't recheck the message id against the cacher here as there might be collisions since we are using
		// a separate sequence counter for direct sender
		messageOk := true
		for index, handler := range handlers {
			errProcess := handler.ProcessReceivedMessage(msg, fromConnectedPeer)
			if errProcess != nil {
				log.Trace("p2p validator",
					"error", errProcess.Error(),
					"topic", msg.Topic(),
					"originator", p2p.MessageOriginatorPid(msg),
					"from connected peer", p2p.PeerIdToShortString(fromConnectedPeer),
					"seq no", p2p.MessageOriginatorSeq(msg),
					"topic identifier", identifiers[index],
				)
				messageOk = false
			}
		}

		netMes.debugger.AddIncomingMessage(msg.Topic(), uint64(len(msg.Data())), !messageOk)

		if messageOk {
			netMes.peersRatingHandler.IncreaseRating(fromConnectedPeer)
		}
	}(msg)

	return nil
}

// IsConnectedToTheNetwork returns true if the current node is connected to the network
func (netMes *networkMessenger) IsConnectedToTheNetwork() bool {
	netw := netMes.p2pHost.Network()
	return netMes.connMonitor.IsConnectedToTheNetwork(netw)
}

// SetThresholdMinConnectedPeers sets the minimum connected peers before triggering a new reconnection
func (netMes *networkMessenger) SetThresholdMinConnectedPeers(minConnectedPeers int) error {
	if minConnectedPeers < 0 {
		return p2p.ErrInvalidValue
	}

	netw := netMes.p2pHost.Network()
	netMes.connMonitor.SetThresholdMinConnectedPeers(minConnectedPeers, netw)

	return nil
}

// ThresholdMinConnectedPeers returns the minimum connected peers before triggering a new reconnection
func (netMes *networkMessenger) ThresholdMinConnectedPeers() int {
	return netMes.connMonitor.ThresholdMinConnectedPeers()
}

// SetPeerShardResolver sets the peer shard resolver component that is able to resolve the link
// between peerID and shardId
func (netMes *networkMessenger) SetPeerShardResolver(peerShardResolver p2p.PeerShardResolver) error {
	if check.IfNil(peerShardResolver) {
		return p2p.ErrNilPeerShardResolver
	}

	err := netMes.sharder.SetPeerShardResolver(peerShardResolver)
	if err != nil {
		return err
	}

	netMes.mutPeerResolver.Lock()
	netMes.peerShardResolver = peerShardResolver
	netMes.mutPeerResolver.Unlock()

	return nil
}

// SetPeerDenialEvaluator sets the peer black list handler
// TODO decide if we continue on using setters or switch to options. Refactor if necessary
func (netMes *networkMessenger) SetPeerDenialEvaluator(handler p2p.PeerDenialEvaluator) error {
	return netMes.connMonitorWrapper.SetPeerDenialEvaluator(handler)
}

// GetConnectedPeersInfo gets the current connected peers information
func (netMes *networkMessenger) GetConnectedPeersInfo() *p2p.ConnectedPeersInfo {
	peers := netMes.p2pHost.Network().Peers()
	connPeerInfo := &p2p.ConnectedPeersInfo{
		UnknownPeers:             make([]string, 0),
		Seeders:                  make([]string, 0),
		IntraShardValidators:     make(map[uint32][]string),
		IntraShardObservers:      make(map[uint32][]string),
		CrossShardValidators:     make(map[uint32][]string),
		CrossShardObservers:      make(map[uint32][]string),
		FullHistoryObservers:     make(map[uint32][]string),
		NumObserversOnShard:      make(map[uint32]int),
		NumValidatorsOnShard:     make(map[uint32]int),
		NumPreferredPeersOnShard: make(map[uint32]int),
	}

	netMes.mutPeerResolver.RLock()
	defer netMes.mutPeerResolver.RUnlock()

	selfPeerInfo := netMes.peerShardResolver.GetPeerInfo(netMes.ID())
	connPeerInfo.SelfShardID = selfPeerInfo.ShardID

	for _, p := range peers {
		conns := netMes.p2pHost.Network().ConnsToPeer(p)
		connString := "[invalid connection string]"
		if len(conns) > 0 {
			connString = conns[0].RemoteMultiaddr().String() + "/p2p/" + p.String()
		}

		pid := core.PeerID(p)
		peerInfo := netMes.peerShardResolver.GetPeerInfo(pid)
		switch peerInfo.PeerType {
		case core.UnknownPeer:
			if netMes.sharder.IsSeeder(pid) {
				connPeerInfo.Seeders = append(connPeerInfo.Seeders, connString)
			} else {
				connPeerInfo.UnknownPeers = append(connPeerInfo.UnknownPeers, connString)
			}
		case core.ValidatorPeer:
			connPeerInfo.NumValidatorsOnShard[peerInfo.ShardID]++
			if selfPeerInfo.ShardID != peerInfo.ShardID {
				connPeerInfo.CrossShardValidators[peerInfo.ShardID] = append(connPeerInfo.CrossShardValidators[peerInfo.ShardID], connString)
				connPeerInfo.NumCrossShardValidators++
			} else {
				connPeerInfo.IntraShardValidators[peerInfo.ShardID] = append(connPeerInfo.IntraShardValidators[peerInfo.ShardID], connString)
				connPeerInfo.NumIntraShardValidators++
			}
		case core.ObserverPeer:
			connPeerInfo.NumObserversOnShard[peerInfo.ShardID]++
			if peerInfo.PeerSubType == core.FullHistoryObserver {
				connPeerInfo.FullHistoryObservers[peerInfo.ShardID] = append(connPeerInfo.FullHistoryObservers[peerInfo.ShardID], connString)
				connPeerInfo.NumFullHistoryObservers++
				break
			}
			if selfPeerInfo.ShardID != peerInfo.ShardID {
				connPeerInfo.CrossShardObservers[peerInfo.ShardID] = append(connPeerInfo.CrossShardObservers[peerInfo.ShardID], connString)
				connPeerInfo.NumCrossShardObservers++
				break
			}

			connPeerInfo.IntraShardObservers[peerInfo.ShardID] = append(connPeerInfo.IntraShardObservers[peerInfo.ShardID], connString)
			connPeerInfo.NumIntraShardObservers++
		}

		if netMes.preferredPeersHolder.Contains(pid) {
			connPeerInfo.NumPreferredPeersOnShard[peerInfo.ShardID]++
		}
	}

	return connPeerInfo
}

// Port returns the port that this network messenger is using
func (netMes *networkMessenger) Port() int {
	return netMes.port
}

// AddPeerTopicNotifier will add a new peer topic notifier
func (netMes *networkMessenger) AddPeerTopicNotifier(notifier p2p.PeerTopicNotifier) error {
	if check.IfNil(notifier) {
		return p2p.ErrNilPeerTopicNotifier
	}

	netMes.mutPeerTopicNotifiers.Lock()
	netMes.peerTopicNotifiers = append(netMes.peerTopicNotifiers, notifier)
	netMes.mutPeerTopicNotifiers.Unlock()

	log.Debug("networkMessenger.AddPeerTopicNotifier", "type", fmt.Sprintf("%T", notifier))

	return nil
}

// IsInterfaceNil returns true if there is no value under the interface
func (netMes *networkMessenger) IsInterfaceNil() bool {
	return netMes == nil
}
