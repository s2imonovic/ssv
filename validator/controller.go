package validator

import (
	"context"
	"encoding/hex"
	"github.com/bloxapp/eth2-key-manager/core"
	"github.com/bloxapp/ssv/beacon"
	"github.com/bloxapp/ssv/eth1"
	"github.com/bloxapp/ssv/eth1/abiparser"
	controller2 "github.com/bloxapp/ssv/ibft/controller"
	"github.com/bloxapp/ssv/network"
	"github.com/bloxapp/ssv/network/p2p"
	"github.com/bloxapp/ssv/operator/forks"
	"github.com/bloxapp/ssv/storage/basedb"
	"github.com/bloxapp/ssv/utils/tasks"
	validatorstorage "github.com/bloxapp/ssv/validator/storage"
	"github.com/herumi/bls-eth-go-binary/bls"
	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/async/event"
	"go.uber.org/zap"
	"sync"
	"time"

	spec "github.com/attestantio/go-eth2-client/spec/phase0"
)

const (
	metadataBatchSize = 25
)

// ControllerOptions for creating a validator controller
type ControllerOptions struct {
	Context                    context.Context
	DB                         basedb.IDb
	Logger                     *zap.Logger
	SignatureCollectionTimeout time.Duration `yaml:"SignatureCollectionTimeout" env:"SIGNATURE_COLLECTION_TIMEOUT" env-default:"5s" env-description:"Timeout for signature collection after consensus"`
	MetadataUpdateInterval     time.Duration `yaml:"MetadataUpdateInterval" env:"METADATA_UPDATE_INTERVAL" env-default:"12m" env-description:"Interval for updating metadata"`
	HistorySyncRateLimit       time.Duration `yaml:"HistorySyncRateLimit" env:"HISTORY_SYNC_BACKOFF" env-default:"200ms" env-description:"Interval for updating metadata"`
	ETHNetwork                 *core.Network
	Network                    network.Network
	Beacon                     beacon.Beacon
	Shares                     []validatorstorage.ShareOptions `yaml:"Shares"`
	ShareEncryptionKeyProvider eth1.ShareEncryptionKeyProvider
	CleanRegistryData          bool
	Fork                       *forks.Forker
	KeyManager                 beacon.KeyManager
}

// Controller represent the validators controller,
// it takes care of bootstrapping, updating and managing existing validators and their shares
type Controller interface {
	ListenToEth1Events(feed *event.Feed)
	ProcessEth1Event(e eth1.Event) error
	StartValidators()
	GetValidatorsIndices() []spec.ValidatorIndex
	GetValidator(pubKey string) (*Validator, bool)
	UpdateValidatorMetaDataLoop()
	StartNetworkMediators()
}

// controller implements Controller
type controller struct {
	context    context.Context
	collection validatorstorage.ICollection
	logger     *zap.Logger
	beacon     beacon.Beacon
	keyManager beacon.KeyManager

	shareEncryptionKeyProvider eth1.ShareEncryptionKeyProvider

	validatorsMap         *validatorsMap
	nonCommitteeValidator controller2.MediatorReader

	metadataUpdateQueue    tasks.Queue
	metadataUpdateInterval time.Duration

	networkMediator controller2.Mediator
	operatorsIDs    *sync.Map
	network         network.Network
}

// NewController creates a new validator controller instance
func NewController(options ControllerOptions) Controller {
	collection := validatorstorage.NewCollection(validatorstorage.CollectionOptions{
		DB:     options.DB,
		Logger: options.Logger,
	})

	// lookup in a map that holds all relevant operators
	operatorsIDs := &sync.Map{}
	notifyOperatorID := func(oid string) {
		operatorsIDs.Store(oid, true)
		// TODO: update network in a better way
		options.Network.NotifyOperatorID(oid)
	}

	ctrl := controller{
		collection:                 collection,
		context:                    options.Context,
		logger:                     options.Logger.With(zap.String("component", "validatorsController")),
		beacon:                     options.Beacon,
		shareEncryptionKeyProvider: options.ShareEncryptionKeyProvider,
		keyManager:                 options.KeyManager,
		network:                    options.Network,

		validatorsMap: newValidatorsMap(options.Context, options.Logger, &Options{
			Context:                    options.Context,
			SignatureCollectionTimeout: options.SignatureCollectionTimeout,
			Logger:                     options.Logger,
			Network:                    options.Network,
			ETHNetwork:                 options.ETHNetwork,
			Beacon:                     options.Beacon,
			DB:                         options.DB,
			Fork:                       options.Fork,
			Signer:                     options.KeyManager,
			SyncRateLimit:              options.HistorySyncRateLimit,
			notifyOperatorID:           notifyOperatorID,
		}),
		nonCommitteeValidator: NewReader(options.Logger.With(zap.String("who", "nonCommitteeReader")), options.DB),

		metadataUpdateQueue:    tasks.NewExecutionQueue(10 * time.Millisecond),
		metadataUpdateInterval: options.MetadataUpdateInterval,

		networkMediator: controller2.NewMediator(options.Logger),
		operatorsIDs:    operatorsIDs,
	}

	if err := ctrl.initShares(options); err != nil {
		ctrl.logger.Panic("could not initialize shares", zap.Error(err))
	}

	return &ctrl
}

// ListenToEth1Events is listening to events coming from eth1 client
func (c *controller) ListenToEth1Events(feed *event.Feed) {
	cn := make(chan *eth1.Event)
	sub := feed.Subscribe(cn)
	defer sub.Unsubscribe()
	for {
		select {
		case e := <-cn:
			if err := c.ProcessOngoingEth1Event(*e); err != nil {
				c.logger.Error("could not process ongoing eth1 event", zap.Error(err))
			}
		case err := <-sub.Err():
			c.logger.Error("event feed subscription error", zap.Error(err))
		}
	}
}

// ProcessOngoingEth1Event handles a single event, will be called in stream events from registry contract
func (c *controller) ProcessOngoingEth1Event(e eth1.Event) error {
	if validatorAddedEvent, ok := e.Data.(abiparser.ValidatorAddedEvent); ok {
		pubKey := hex.EncodeToString(validatorAddedEvent.PublicKey)
		if _, ok := c.validatorsMap.GetValidator(pubKey); ok {
			c.logger.Debug("validator was loaded already")
			return nil
		}
		share, err := c.handleValidatorAddedEvent(validatorAddedEvent)
		if err != nil {
			c.logger.Error("could not handle validatorAdded event", zap.String("pubkey", pubKey), zap.Error(err))
			return err
		}
		v := c.validatorsMap.GetOrCreateValidator(share)
		if err := c.startValidator(v); err != nil {
			c.logger.Warn("could not start validator", zap.Error(err))
		}
	}
	return nil
}

// ProcessEth1Event handles a single event, will be called in sync events from registry contract
func (c *controller) ProcessEth1Event(e eth1.Event) error {
	if validatorAddedEvent, ok := e.Data.(abiparser.ValidatorAddedEvent); ok {
		pubKey := hex.EncodeToString(validatorAddedEvent.PublicKey)
		_, err := c.handleValidatorAddedEvent(validatorAddedEvent)
		if err != nil {
			c.logger.Error("could not process validator", zap.String("pubkey", pubKey), zap.Error(err))
			return err
		}
	}
	return nil
}

// StartValidators loads all persisted shares and setup the corresponding validators
func (c *controller) StartValidators() {
	shares, err := c.collection.GetAllValidatorsShare()
	if err != nil {
		c.logger.Fatal("failed to get validators shares", zap.Error(err))
	}
	if len(shares) == 0 {
		c.logger.Info("could not find validators")
		return
	}
	c.setupValidators(shares)
	// inject handler for finding relevant operators
	p2p.UseLookupOperatorHandler(c.network, func(oid string) bool {
		_, ok := c.operatorsIDs.Load(oid)
		return ok
	})
	// print current relevant operators (ids)
	ids := []string{}
	c.operatorsIDs.Range(func(key, value interface{}) bool {
		ids = append(ids, key.(string))
		return true
	})
	c.logger.Debug("relevant operators", zap.Int("len", len(ids)), zap.Strings("op_ids", ids))
}

// setupValidators setup and starts validators from the given shares
// shares w/o validator's metadata won't start, but the metadata will be fetched and the validator will start afterwards
func (c *controller) setupValidators(shares []*validatorstorage.Share) {
	c.logger.Info("starting validators setup...", zap.Int("shares count", len(shares)))
	var started int
	var errs []error
	var fetchMetadata [][]byte
	for _, validatorShare := range shares {
		v := c.validatorsMap.GetOrCreateValidator(validatorShare)
		pk := v.Share.PublicKey.SerializeToHexStr()
		logger := c.logger.With(zap.String("pubkey", pk))
		if !v.Share.HasMetadata() { // fetching index and status in case not exist
			fetchMetadata = append(fetchMetadata, v.Share.PublicKey.Serialize())
			logger.Warn("could not start validator as metadata not found")
			continue
		}
		if err := c.startValidator(v); err != nil {
			logger.Warn("could not start validator", zap.Error(err))
			errs = append(errs, err)
		} else {
			started++
		}
	}
	c.logger.Info("setup validators done", zap.Int("map size", c.validatorsMap.Size()),
		zap.Int("failures", len(errs)), zap.Int("missing metadata", len(fetchMetadata)),
		zap.Int("shares count", len(shares)), zap.Int("started", started))

	go c.updateValidatorsMetadata(fetchMetadata)
}

func (c *controller) StartNetworkMediators() {
	msgChan, msgDone := c.validatorsMap.optsTemplate.Network.ReceivedMsgChan()
	decidedChan, decidedDone := c.validatorsMap.optsTemplate.Network.ReceivedDecidedChan()

	c.networkMediator.AddListener(network.NetworkMsg_IBFTType, msgChan, msgDone, c.getReader)
	c.networkMediator.AddListener(network.NetworkMsg_DecidedType, decidedChan, decidedDone, c.getReader)
}

func (c *controller) getReader(publicKey string) (controller2.MediatorReader, bool) {
	v, ok := c.validatorsMap.GetValidator(publicKey)
	if !ok { //TODo after fork?
		//	return handler for non committee validator to save the decided
		return c.nonCommitteeValidator, true
	}
	return v, ok
}

// updateValidatorsMetadata updates metadata of the given public keys.
// as part of the flow in beacon.UpdateValidatorsMetadata,
// UpdateValidatorMetadata is called to persist metadata and start a specific validator
func (c *controller) updateValidatorsMetadata(pubKeys [][]byte) {
	if len(pubKeys) > 0 {
		c.logger.Debug("updating validators", zap.Int("count", len(pubKeys)))
		if err := beacon.UpdateValidatorsMetadata(pubKeys, c, c.beacon, c.onMetadataUpdated); err != nil {
			c.logger.Error("could not update all validators", zap.Error(err))
		}
	}
}

// UpdateValidatorMetadata updates a given validator with metadata (implements ValidatorMetadataStorage)
func (c *controller) UpdateValidatorMetadata(pk string, metadata *beacon.ValidatorMetadata) error {
	if metadata == nil {
		return errors.New("could not update empty metadata")
	}
	if v, found := c.validatorsMap.GetValidator(pk); found {
		v.Share.Metadata = metadata
		if err := c.collection.(beacon.ValidatorMetadataStorage).UpdateValidatorMetadata(pk, metadata); err != nil {
			return err
		}
		if err := c.startValidator(v); err != nil {
			c.logger.Error("could not start validator", zap.Error(err))
		}
	}
	return nil
}

// GetValidator returns a validator instance from validatorsMap
func (c *controller) GetValidator(pubKey string) (*Validator, bool) {
	return c.validatorsMap.GetValidator(pubKey)
}

// GetValidatorsIndices returns a list of all the active validators indices
// and fetch indices for missing once (could be first time attesting or non active once)
func (c *controller) GetValidatorsIndices() []spec.ValidatorIndex {
	var toFetch [][]byte
	var indices []spec.ValidatorIndex

	err := c.validatorsMap.ForEach(func(v *Validator) error {
		if !v.Share.HasMetadata() {
			toFetch = append(toFetch, v.Share.PublicKey.Serialize())
		} else if v.Share.Metadata.IsActive() { // eth-client throws error once trying to fetch duties for existed validator
			indices = append(indices, v.Share.Metadata.Index)
		}
		return nil
	})
	if err != nil {
		c.logger.Error("failed to get all validators public keys", zap.Error(err))
	}

	go c.updateValidatorsMetadata(toFetch)

	return indices
}

// handleValidatorAddedEvent handles registry contract event for validator added
func (c *controller) handleValidatorAddedEvent(validatorAddedEvent abiparser.ValidatorAddedEvent) (*validatorstorage.Share, error) {
	pubKey := hex.EncodeToString(validatorAddedEvent.PublicKey)
	logger := c.logger.With(zap.String("pubKey", pubKey))
	logger.Debug("new validator, starting setup")
	metricsValidatorStatus.WithLabelValues(pubKey).Set(float64(validatorStatusInactive))
	validatorShare, found, err := c.collection.GetValidatorShare(validatorAddedEvent.PublicKey)
	if err != nil {
		return nil, errors.Wrap(err, "could not check if validator share exits")
	}
	if !found {
		newValShare, share, err := createShareWithOperatorKey(validatorAddedEvent, c.shareEncryptionKeyProvider)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create share")
		}
		if err := c.onNewShare(newValShare, share); err != nil {
			metricsValidatorStatus.WithLabelValues(pubKey).Set(float64(validatorStatusError))
			return nil, err
		}
		validatorShare = newValShare
		logger.Debug("new validator share was created and saved")
	}
	return validatorShare, nil
}

// onMetadataUpdated is called when validator's metadata was updated
func (c *controller) onMetadataUpdated(pk string, meta *beacon.ValidatorMetadata) {
	if meta == nil {
		return
	}
	if v, exist := c.GetValidator(pk); exist {
		// update share object owned by the validator
		// TODO: check if this updates running validators
		if !v.Share.HasMetadata() {
			v.Share.Metadata = meta
			c.logger.Debug("metadata was updated", zap.String("pk", pk))
		} else if !v.Share.Metadata.Equals(meta) {
			v.Share.Metadata.Status = meta.Status
			v.Share.Metadata.Balance = meta.Balance
			c.logger.Debug("metadata was updated", zap.String("pk", pk))
		}
		if err := c.startValidator(v); err != nil {
			c.logger.Error("could not start validator after metadata update",
				zap.String("pk", pk), zap.Error(err), zap.Any("metadata", meta))
		}
	}
}

// onNewShare is called when a new validator was added or during registry sync
// if the validator was persisted already, this function won't be called
func (c *controller) onNewShare(share *validatorstorage.Share, shareSecret *bls.SecretKey) error {
	logger := c.logger.With(zap.String("pubKey", share.PublicKey.SerializeToHexStr()))
	if updated, err := UpdateShareMetadata(share, c.beacon); err != nil {
		logger.Warn("could not add validator metadata", zap.Error(err))
	} else if !updated {
		logger.Warn("could not find validator metadata")
	} else {
		logger.Debug("validator metadata was updated")
	}
	// save secret key
	if err := c.keyManager.AddShare(shareSecret); err != nil {
		return errors.Wrap(err, "failed to save new share secret to key manager")
	}
	logger.Info("share was added successfully to key manager")

	// save validator data
	if err := c.collection.SaveValidatorShare(share); err != nil {
		return errors.Wrap(err, "failed to save new share")
	}
	return nil
}

// startValidator will start the given validator if applicable
func (c *controller) startValidator(v *Validator) error {
	ReportValidatorStatus(v.Share.PublicKey.SerializeToHexStr(), v.Share.Metadata, c.logger)
	if !v.Share.HasMetadata() {
		return errors.New("could not start validator: metadata not found")
	}
	if v.Share.Metadata.Index == 0 {
		return errors.New("could not start validator: index not found")
	}
	if err := v.Start(); err != nil {
		metricsValidatorStatus.WithLabelValues(v.Share.PublicKey.SerializeToHexStr()).Set(float64(validatorStatusError))
		return errors.Wrap(err, "could not start validator")
	}
	return nil
}

// UpdateValidatorMetaDataLoop updates metadata of validators in an interval
func (c *controller) UpdateValidatorMetaDataLoop() {
	go c.metadataUpdateQueue.Start()

	for {
		time.Sleep(c.metadataUpdateInterval)

		shares, err := c.collection.GetAllValidatorsShare()
		if err != nil {
			c.logger.Error("could not get validators shares for metadata update", zap.Error(err))
			continue
		}
		var pks [][]byte
		for _, share := range shares {
			pks = append(pks, share.PublicKey.Serialize())
		}
		c.logger.Debug("updating metadata in loop", zap.Int("shares count", len(shares)))
		beacon.UpdateValidatorsMetadataBatch(pks, c.metadataUpdateQueue, c,
			c.beacon, c.onMetadataUpdated, metadataBatchSize)
	}
}
