/*
 * Copyright (C) 2019 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package pingpong

import (
	"encoding/hex"
	"encoding/json"
	stdErr "errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/mysteriumnetwork/node/config"
	"github.com/mysteriumnetwork/node/core/node/event"
	"github.com/mysteriumnetwork/node/core/service/servicestate"
	"github.com/mysteriumnetwork/node/eventbus"
	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/identity/registry"
	sessionEvent "github.com/mysteriumnetwork/node/session/event"
	pinge "github.com/mysteriumnetwork/node/session/pingpong/event"
	"github.com/mysteriumnetwork/payments/crypto"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

type hermesPromiseStorage interface {
	Store(promise HermesPromise) error
}

type feeProvider interface {
	FetchSettleFees(chainID int64) (registry.FeesResponse, error)
}

// HermesHTTPRequester represents HTTP requests to Hermes.
type HermesHTTPRequester interface {
	RequestPromise(rp RequestPromise) (crypto.Promise, error)
	RevealR(r string, provider string, agreementID *big.Int) error
	UpdatePromiseFee(promise crypto.Promise, newFee *big.Int) (crypto.Promise, error)
}

type encryption interface {
	Decrypt(addr common.Address, encrypted []byte) ([]byte, error)
	Encrypt(addr common.Address, plaintext []byte) ([]byte, error)
}

// HermesCallerFactory represents Hermes caller factory.
type HermesCallerFactory func(url string) HermesHTTPRequester

// HermesPromiseHandlerDeps represents the HermesPromiseHandler dependencies.
type HermesPromiseHandlerDeps struct {
	HermesPromiseStorage hermesPromiseStorage
	FeeProvider          feeProvider
	Encryption           encryption
	EventBus             eventbus.Publisher
	HermesURLGetter      hermesURLGetter
	HermesCallerFactory  HermesCallerFactory
}

// HermesPromiseHandler handles the hermes promises for ongoing sessions.
type HermesPromiseHandler struct {
	deps          HermesPromiseHandlerDeps
	queue         chan enqueuedRequest
	stop          chan struct{}
	stopOnce      sync.Once
	startOnce     sync.Once
	transactorFee registry.FeesResponse
}

// NewHermesPromiseHandler returns a new instance of hermes promise handler.
func NewHermesPromiseHandler(deps HermesPromiseHandlerDeps) *HermesPromiseHandler {
	return &HermesPromiseHandler{
		deps:  deps,
		queue: make(chan enqueuedRequest, 100),
		stop:  make(chan struct{}),
	}
}

type enqueuedRequest struct {
	errChan    chan error
	r          []byte
	em         crypto.ExchangeMessage
	providerID identity.Identity
	sessionID  string
}

type hermesURLGetter interface {
	GetHermesURL(address common.Address) (string, error)
}

// RequestPromise adds the request to the queue.
func (aph *HermesPromiseHandler) RequestPromise(r []byte, em crypto.ExchangeMessage, providerID identity.Identity, sessionID string) <-chan error {
	er := enqueuedRequest{
		r:          r,
		em:         em,
		providerID: providerID,
		errChan:    make(chan error),
		sessionID:  sessionID,
	}
	aph.queue <- er
	return er.errChan
}

func (aph *HermesPromiseHandler) updateFee() {
	fees, err := aph.deps.FeeProvider.FetchSettleFees(config.GetInt64(config.FlagChainID))
	if err != nil {
		log.Warn().Err(err).Msg("could not fetch fees, ignoring")
		return
	}

	aph.transactorFee = fees
}

func (aph *HermesPromiseHandler) handleRequests() {
	log.Debug().Msgf("hermes promise handler started")
	defer log.Debug().Msgf("hermes promise handler stopped")
	for {
		select {
		case <-aph.stop:
			return
		case entry := <-aph.queue:
			aph.requestPromise(entry)
		}
	}
}

// Subscribe subscribes HermesPromiseHandler to relevant events.
func (aph *HermesPromiseHandler) Subscribe(bus eventbus.Subscriber) error {
	err := bus.SubscribeAsync(event.AppTopicNode, aph.handleNodeStopEvents)
	if err != nil {
		return fmt.Errorf("could not subscribe to node events: %w", err)
	}

	err = bus.SubscribeAsync(servicestate.AppTopicServiceStatus, aph.handleServiceEvent)
	if err != nil {
		return fmt.Errorf("could not subscribe to service events: %w", err)
	}
	return nil
}

func (aph *HermesPromiseHandler) handleServiceEvent(ev servicestate.AppEventServiceStatus) {
	if ev.Status == string(servicestate.Running) {
		aph.startOnce.Do(
			func() {
				aph.updateFee()
				aph.handleRequests()
			})
	}
}

func (aph *HermesPromiseHandler) doStop() {
	aph.stopOnce.Do(func() {
		close(aph.stop)
	})
}

func (aph *HermesPromiseHandler) handleNodeStopEvents(e event.Payload) {
	if e.Status == event.StatusStopped {
		aph.doStop()
		return
	}
}

func (aph *HermesPromiseHandler) requestPromise(er enqueuedRequest) {
	defer close(er.errChan)

	providerID := er.providerID
	hermesID := common.HexToAddress(er.em.HermesID)
	channelID, err := crypto.GenerateProviderChannelID(providerID.Address, hermesID.Hex())
	if err != nil {
		er.errChan <- fmt.Errorf("could not generate provider channel address: %w", err)
		return
	}

	if !aph.transactorFee.IsValid() {
		aph.updateFee()
	}

	details := rRecoveryDetails{
		R:           hex.EncodeToString(er.r),
		AgreementID: er.em.AgreementID,
	}

	bytes, err := json.Marshal(details)
	if err != nil {
		er.errChan <- fmt.Errorf("could not marshal R recovery details: %w", err)
		return
	}

	encrypted, err := aph.deps.Encryption.Encrypt(providerID.ToCommonAddress(), bytes)
	if err != nil {
		er.errChan <- fmt.Errorf("could not encrypt R: %w", err)
		return
	}

	request := RequestPromise{
		ExchangeMessage: er.em,
		TransactorFee:   aph.transactorFee.Fee,
		RRecoveryData:   hex.EncodeToString(encrypted),
	}

	hermesCaller, err := aph.getHermesCaller(hermesID)
	if err != nil {
		er.errChan <- fmt.Errorf("could not get hermes caller: %w", err)
		return
	}
	promise, err := hermesCaller.RequestPromise(request)
	err = aph.handleHermesError(err, providerID, hermesID)
	if err != nil {
		er.errChan <- fmt.Errorf("hermes request promise error: %w", err)
		return
	}

	if promise.ChainID != request.ExchangeMessage.ChainID {
		log.Debug().Msgf("Received promise with wrong chain id from hermes. Expected %v, got %v", request.ExchangeMessage.ChainID, promise.ChainID)
	}

	ap := HermesPromise{
		ChannelID:   channelID,
		Identity:    providerID,
		HermesID:    hermesID,
		Promise:     promise,
		R:           hex.EncodeToString(er.r),
		Revealed:    false,
		AgreementID: er.em.AgreementID,
	}

	err = aph.deps.HermesPromiseStorage.Store(ap)
	if err != nil && !stdErr.Is(err, ErrAttemptToOverwrite) {
		er.errChan <- fmt.Errorf("could not store hermes promise: %w", err)
		return
	}

	aph.deps.EventBus.Publish(pinge.AppTopicHermesPromise, pinge.AppEventHermesPromise{
		Promise:    promise,
		HermesID:   hermesID,
		ProviderID: providerID,
	})
	aph.deps.EventBus.Publish(sessionEvent.AppTopicTokensEarned, sessionEvent.AppEventTokensEarned{
		ProviderID: providerID,
		SessionID:  er.sessionID,
		Total:      er.em.AgreementTotal,
	})

	err = aph.revealR(ap)
	err = aph.handleHermesError(err, providerID, hermesID)
	if err != nil {
		er.errChan <- fmt.Errorf("hermes reveal r error: %w", err)
		return
	}
}

func (aph *HermesPromiseHandler) getHermesCaller(hermesID common.Address) (HermesHTTPRequester, error) {
	addr, err := aph.deps.HermesURLGetter.GetHermesURL(hermesID)
	if err != nil {
		return nil, fmt.Errorf("could not get hermes URL: %w", err)
	}
	return aph.deps.HermesCallerFactory(addr), nil
}

func (aph *HermesPromiseHandler) revealR(hermesPromise HermesPromise) error {
	if hermesPromise.Revealed {
		return nil
	}

	hermesCaller, err := aph.getHermesCaller(hermesPromise.HermesID)
	if err != nil {
		return fmt.Errorf("could not get hermes caller: %w", err)
	}

	err = hermesCaller.RevealR(hermesPromise.R, hermesPromise.Identity.Address, hermesPromise.AgreementID)
	handledErr := aph.handleHermesError(err, hermesPromise.Identity, hermesPromise.HermesID)
	if handledErr != nil {
		return fmt.Errorf("could not reveal R: %w", err)
	}

	hermesPromise.Revealed = true
	err = aph.deps.HermesPromiseStorage.Store(hermesPromise)
	if err != nil && !stdErr.Is(err, ErrAttemptToOverwrite) {
		return fmt.Errorf("could not store hermes promise: %w", err)
	}

	return nil
}

func (aph *HermesPromiseHandler) handleHermesError(err error, providerID identity.Identity, hermesID common.Address) error {
	if err == nil {
		return nil
	}

	switch {
	case stdErr.Is(err, ErrNeedsRRecovery):
		var aer HermesErrorResponse
		ok := stdErr.As(err, &aer)
		if !ok {
			return errors.New("could not cast errNeedsRecovery to hermesError")
		}
		recoveryErr := aph.recoverR(aer, providerID, hermesID)
		if recoveryErr != nil {
			return recoveryErr
		}
		return nil
	case stdErr.Is(err, ErrHermesNoPreviousPromise):
		log.Info().Msg("no previous promise on hermes, will mark R as revealed")
		return nil
	default:
		return err
	}
}

func (aph *HermesPromiseHandler) recoverR(aerr hermesError, providerID identity.Identity, hermesID common.Address) error {
	log.Info().Msg("Recovering R...")
	decoded, err := hex.DecodeString(aerr.Data())
	if err != nil {
		return fmt.Errorf("could not decode R recovery details: %w", err)
	}

	decrypted, err := aph.deps.Encryption.Decrypt(providerID.ToCommonAddress(), decoded)
	if err != nil {
		return fmt.Errorf("could not decrypt R details: %w", err)
	}

	res := rRecoveryDetails{}
	err = json.Unmarshal(decrypted, &res)
	if err != nil {
		return fmt.Errorf("could not unmarshal R details: %w", err)
	}

	log.Info().Msg("R recovered, will reveal...")
	hermesCaller, err := aph.getHermesCaller(hermesID)
	if err != nil {
		return fmt.Errorf("could not get hermes caller: %w", err)
	}

	err = hermesCaller.RevealR(res.R, providerID.Address, res.AgreementID)
	if err != nil {
		return fmt.Errorf("could not reveal R: %w", err)
	}

	log.Info().Msg("R recovered successfully")
	return nil
}
