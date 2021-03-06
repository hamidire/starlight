package fsm

import (
	"encoding/json"
	"time"

	b "github.com/stellar/go/build"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/xdr"

	"github.com/interstellar/starlight/errors"
	"github.com/interstellar/starlight/starlight/key"
	"github.com/interstellar/starlight/worizon/xlm"
)

// version denotes the current Starlight protocol version.
const version = 2

// Message defines a JSON schema for Starlight messages.
type Message struct {
	ChannelID string
	MsgNum    uint64
	Version   int

	ChannelProposeMsg  *ChannelProposeMsg  `json:",omitempty"`
	ChannelAcceptMsg   *ChannelAcceptMsg   `json:",omitempty"`
	PaymentProposeMsg  *PaymentProposeMsg  `json:",omitempty"`
	PaymentAcceptMsg   *PaymentAcceptMsg   `json:",omitempty"`
	PaymentCompleteMsg *PaymentCompleteMsg `json:",omitempty"`
	CloseMsg           *CloseMsg           `json:",omitempty"`

	// Signature is a signature over the JSON representation of the message
	// (minus the Signature field itself), made with the sender's key.
	Signature []byte `json:",omitempty"`
}

// MarshalJSON implements json.Marshaler. Required for genbolt.
func (m *Message) MarshalJSON() ([]byte, error) {
	type t Message
	return json.Marshal((*t)(m))
}

// UnmarshalJSON implements json.Unmarshaler. Required for genbolt.
func (m *Message) UnmarshalJSON(b []byte) error {
	type t Message
	return json.Unmarshal(b, (*t)(m))
}

// ChannelProposeMsg defines a JSON schema for proposal over a Channel.
type ChannelProposeMsg struct {
	HostAcct           AccountID
	GuestAcct          AccountID
	HostRatchetAcct    AccountID
	GuestRatchetAcct   AccountID
	MaxRoundDuration   time.Duration
	FinalityDelay      time.Duration
	BaseSequenceNumber xdr.SequenceNumber
	HostAmount         xlm.Amount
	Feerate            xlm.Amount
	FundingTime        time.Time
}

// ChannelAcceptMsg contains Signatures for Guest accepting a proposal.
type ChannelAcceptMsg struct {
	GuestRatchetRound1Sig      xdr.DecoratedSignature
	GuestSettleOnlyWithHostSig xdr.DecoratedSignature
}

// PaymentProposeMsg is the protocol message proposing a channel payment.
type PaymentProposeMsg struct {
	RoundNumber              uint64
	PaymentTime              time.Time
	PaymentAmount            xlm.Amount
	SenderSettleWithGuestSig xdr.DecoratedSignature
	SenderSettleWithHostSig  xdr.DecoratedSignature
}

// PaymentAcceptMsg is the protocol message accepting a proposed channel payment.
type PaymentAcceptMsg struct {
	RoundNumber                 uint64
	RecipientRatchetSig         xdr.DecoratedSignature
	RecipientSettleWithGuestSig *xdr.DecoratedSignature
	RecipientSettleWithHostSig  xdr.DecoratedSignature
}

// PaymentCompleteMsg is the protocol message acknowledging a PaymentAcceptMsg.
type PaymentCompleteMsg struct {
	RoundNumber      uint64
	SenderRatchetSig xdr.DecoratedSignature
}

// CloseMsg is the protocol message proposing a cooperative closure of the channel.
type CloseMsg struct {
	CooperativeCloseSig xdr.DecoratedSignature
}

func (u *Updater) handlePaymentCompleteMsg(m *Message) error {
	if u.C.State != PaymentAccepted {
		return errors.Wrap(ErrUnexpectedState, u.C.State)
	}
	var (
		senderRatchetAccount AccountID
		senderRatchetSeqNum  xdr.SequenceNumber
		senderKey            keypair.KP
		err                  error
	)
	delta := u.C.PendingAmountReceived - u.C.PendingAmountSent
	switch u.C.Role {
	case Guest:
		u.C.GuestAmount += delta
		u.C.HostAmount -= delta
		senderRatchetAccount = u.C.HostRatchetAcct
		senderRatchetSeqNum = u.C.HostRatchetAcctSeqNum
		senderKey, err = keypair.Parse(u.C.EscrowAcct.Address())
		if err != nil {
			return err
		}
	case Host:
		u.C.HostAmount += delta
		u.C.GuestAmount -= delta
		senderRatchetAccount = u.C.GuestRatchetAcct
		senderRatchetSeqNum = u.C.GuestRatchetAcctSeqNum
		senderKey, err = keypair.Parse(u.C.GuestAcct.Address())
		if err != nil {
			return err
		}
	}
	ratchetTx, err := buildRatchetTx(u.C, u.C.PendingPaymentTime, senderRatchetAccount, senderRatchetSeqNum)
	if err != nil {
		return err
	}
	complete := m.PaymentCompleteMsg
	if err = verifySig(ratchetTx, senderKey, complete.SenderRatchetSig); err != nil {
		return errors.Wrap(err, "ratchet tx")
	}
	u.C.CurrentSettleWithGuestTx = u.C.CounterpartyLatestSettleWithGuestTx
	u.C.CurrentSettleWithHostTx = u.C.CounterpartyLatestSettleWithHostTx
	err = u.C.signRatchetTx(ratchetTx, complete.SenderRatchetSig, u.Seed)
	if err != nil {
		return err
	}
	u.C.PaymentTime = u.C.PendingPaymentTime
	u.C.PendingAmountReceived = 0
	u.C.PendingAmountSent = 0
	return u.transitionTo(Open)
}

func (u *Updater) handlePaymentAcceptMsg(m *Message) error {
	accept := m.PaymentAcceptMsg

	var (
		err              error
		recipientAccount AccountID
		recipientSeqNum  xdr.SequenceNumber
		recipientKey     keypair.KP
	)

	switch u.C.Role {
	case Guest:
		u.C.GuestAmount -= u.C.PendingAmountSent
		u.C.HostAmount += u.C.PendingAmountSent
		recipientAccount = u.C.HostRatchetAcct
		recipientSeqNum = u.C.HostRatchetAcctSeqNum
		recipientKey, err = keypair.Parse(u.C.EscrowAcct.Address())
	case Host:
		u.C.HostAmount -= u.C.PendingAmountSent
		u.C.GuestAmount += u.C.PendingAmountSent
		recipientAccount = u.C.GuestRatchetAcct
		recipientSeqNum = u.C.GuestRatchetAcctSeqNum
		recipientKey, err = keypair.Parse(u.C.GuestAcct.Address())
	}
	if err != nil {
		return err
	}
	ratchetTx, err := buildRatchetTx(u.C, u.C.PendingPaymentTime, recipientAccount, recipientSeqNum)
	if err != nil {
		return err
	}
	if err = verifySig(ratchetTx, recipientKey, accept.RecipientRatchetSig); err != nil {
		return errors.Wrap(err, "ratchet tx")
	}

	var hostTx *b.TransactionBuilder

	if u.C.GuestAmount == 0 {
		hostTx, err = buildSettleOnlyWithHostTx(u.C, u.C.PendingPaymentTime)
	} else {
		hostTx, err = buildSettleWithHostTx(u.C, u.C.PendingPaymentTime)
	}
	if err != nil {
		return err
	}

	if err = verifySig(hostTx, recipientKey, accept.RecipientSettleWithHostSig); err != nil {
		return errors.Wrap(err, "settle with host tx")
	}

	var guestTx *b.TransactionBuilder
	var recipientSettleWithGuestSig *xdr.DecoratedSignature
	if u.C.GuestAmount == 0 {
		if accept.RecipientSettleWithGuestSig != nil {
			return ErrUnusedSettleWithGuestSig
		}
	} else {
		guestTx, err = buildSettleWithGuestTx(u.C, u.C.PendingPaymentTime)
		if err != nil {
			return errors.Wrap(err, "building settle with guest tx")
		}
		if err = verifySig(guestTx, recipientKey, *accept.RecipientSettleWithGuestSig); err != nil {
			return errors.Wrap(err, "settle with guest tx")
		}
		recipientSettleWithGuestSig = accept.RecipientSettleWithGuestSig
	}

	// Sets the counterparty and latest settlement txes
	err = u.C.setLatestSettlementTxes(guestTx, hostTx, recipientSettleWithGuestSig,
		accept.RecipientSettleWithHostSig, u.Seed)
	if err != nil {
		return err
	}
	err = u.C.signRatchetTx(ratchetTx, accept.RecipientRatchetSig, u.Seed)
	if err != nil {
		return err
	}
	u.C.PaymentTime = u.C.PendingPaymentTime
	u.C.PendingAmountReceived = 0
	u.C.PendingAmountSent = 0
	return u.transitionTo(Open)
}

func (u *Updater) handleChannelProposeMsg(m *Message) error {
	propose := m.ChannelProposeMsg

	if u.C.State != Start {
		return ErrChannelExists
	}

	if !propose.GuestAcct.Equals(u.C.GuestAcct) {
		u.debugf("dropped message: proposed guest acct %s doesn't match channel guest acct %s", propose.GuestAcct.Address(), u.C.GuestAcct.Address())
		return nil
	}

	var EscrowAcct AccountID
	err := EscrowAcct.SetAddress(string(m.ChannelID))
	if err != nil {
		return err
	}
	*u.C = Channel{
		ID:                     m.ChannelID,
		Role:                   Guest,
		HostAmount:             propose.HostAmount,
		MaxRoundDuration:       propose.MaxRoundDuration,
		FinalityDelay:          propose.FinalityDelay,
		FundingTime:            propose.FundingTime,
		PaymentTime:            propose.FundingTime,
		HostAcct:               propose.HostAcct,
		GuestAcct:              u.C.GuestAcct,
		EscrowAcct:             EscrowAcct,
		HostRatchetAcct:        propose.HostRatchetAcct,
		GuestRatchetAcct:       propose.GuestRatchetAcct,
		RoundNumber:            1,
		BaseSequenceNumber:     u.C.BaseSequenceNumber,
		HostRatchetAcctSeqNum:  u.C.HostRatchetAcctSeqNum,
		GuestRatchetAcctSeqNum: u.C.GuestRatchetAcctSeqNum,
		KeyIndex:               key.PrimaryAccountIndex,
		Passphrase:             u.Passphrase,
		CounterpartyAddress:    u.C.CounterpartyAddress,
		ChannelFeerate:         propose.Feerate,
	}

	return u.transitionTo(AwaitingFunding)
}

func (u *Updater) handleChannelAcceptMsg(m *Message) error {
	accept := m.ChannelAcceptMsg
	if u.C.State != ChannelProposed {
		return errors.Wrap(ErrUnexpectedState, u.C.State)
	}
	if u.C.Role != Host {
		u.debugf("dropped message: host cannot accept channel")
		return nil
	}
	if u.LedgerTime.After(u.C.FundingTime.Add(u.C.MaxRoundDuration)) {
		u.debugf("dropped message: ledger time %s past funding time %s with max round duration %s", u.LedgerTime, u.C.FundingTime, u.C.MaxRoundDuration)
		return nil
	}
	u.H.Seqnum++

	guestKey, err := keypair.Parse(u.C.GuestAcct.Address())
	if err != nil {
		return err
	}

	ratchetTx, err := buildRatchetTx(u.C, u.C.FundingTime, u.C.HostRatchetAcct, u.C.HostRatchetAcctSeqNum)
	if err != nil {
		return err
	}
	if err := verifySig(ratchetTx, guestKey, accept.GuestRatchetRound1Sig); err != nil {
		return errors.Wrap(err, "invalid signature on round 1 ratchet tx")
	}

	// Set current ratchet tx
	u.C.signRatchetTx(ratchetTx, accept.GuestRatchetRound1Sig, u.Seed)

	settleOnlyWithHostTx, err := buildSettleOnlyWithHostTx(u.C, u.C.FundingTime)
	if err != nil {
		return err
	}
	if err := verifySig(settleOnlyWithHostTx, guestKey, accept.GuestSettleOnlyWithHostSig); err != nil {
		return errors.Wrap(err, "invalid signature on round 1 settlement tx")
	}
	// Set current settlement tx
	u.C.setLatestSettlementTxes(nil, settleOnlyWithHostTx, nil, accept.GuestSettleOnlyWithHostSig, u.Seed)

	return u.transitionTo(AwaitingFunding)
}

func (u *Updater) handlePaymentProposeMsg(m *Message) error {
	payment := m.PaymentProposeMsg
	switch u.C.State {
	case Open, PaymentProposed, AwaitingPaymentMerge:
		// Accepted states
	default:
		return errors.Wrap(ErrUnexpectedState, u.C.State)
	}
	if payment.PaymentAmount < 0 {
		u.debugf("dropped message: invalid payment amount %s", payment.PaymentAmount)
		return nil
	}
	var verifyKey keypair.KP
	var err error
	switch u.C.Role {
	case Guest:
		if payment.PaymentAmount > u.C.HostAmount {
			u.debugf("dropped message: invalid payment amount %s from host with balance %s", payment.PaymentAmount, u.C.HostAmount)
			return nil
		}
		verifyKey, err = keypair.Parse(u.C.EscrowAcct.Address())
		if err != nil {
			return err
		}
	case Host:
		if payment.PaymentAmount > u.C.GuestAmount {
			u.debugf("dropped message: invalid payment amount %s from guest with balance %s", payment.PaymentAmount, u.C.HostAmount)
			return nil
		}
		verifyKey, err = keypair.Parse(u.C.GuestAcct.Address())
		if err != nil {
			return err
		}
	}
	// Verify signatures
	ch2 := *u.C

	if u.C.State == Open || u.C.State == AwaitingPaymentMerge {
		ch2.RoundNumber++
	}

	switch ch2.Role {
	case Guest:
		ch2.GuestAmount += payment.PaymentAmount
		ch2.HostAmount -= payment.PaymentAmount
	case Host:
		ch2.HostAmount += payment.PaymentAmount
		ch2.GuestAmount -= payment.PaymentAmount
	}

	var settleWithHostTx, settleWithGuestTx *b.TransactionBuilder
	if ch2.GuestAmount == 0 {
		if payment.SenderSettleWithGuestSig.Signature != nil {
			u.debugf("dropped message: %s", ErrUnusedSettleWithGuestSig)
			return ErrUnusedSettleWithGuestSig
		}
		settleWithHostTx, err = buildSettleOnlyWithHostTx(&ch2, payment.PaymentTime)
		if err != nil {
			u.debugf("dropped message: error building SettleOnlyWithHostTx %s", err)
			return err
		}
	} else {
		settleWithGuestTx, err = buildSettleWithGuestTx(&ch2, payment.PaymentTime)
		if err != nil {
			u.debugf("dropped message: error building SettleWithGuestTx %s", err)
			return err
		}
		if err = verifySig(settleWithGuestTx, verifyKey, payment.SenderSettleWithGuestSig); err != nil {
			return errors.Wrap(err, "settle with guest tx")
		}
		settleWithHostTx, err = buildSettleWithHostTx(&ch2, payment.PaymentTime)
		if err != nil {
			u.debugf("dropped message: error building SettleWithHostTx %s", err)
			return err
		}
	}
	if err = verifySig(settleWithHostTx, verifyKey, payment.SenderSettleWithHostSig); err != nil {
		return errors.Wrap(err, "settle with host tx")
	}

	switch u.C.State {
	case Open, AwaitingPaymentMerge:
		if u.C.RoundNumber >= payment.RoundNumber {
			u.debugf("dropped message: payment round %d for channel round %d", payment.RoundNumber, u.C.RoundNumber)
			return nil
		}
		if u.LedgerTime.After(payment.PaymentTime.Add(u.C.MaxRoundDuration)) {
			u.debugf("dropped message: payment time %v with duration %v at ledger time %v", payment.PaymentTime, u.C.MaxRoundDuration, u.LedgerTime)
			return nil
		}
		if u.LedgerTime.Before(payment.PaymentTime.Add(-1 * u.C.MaxRoundDuration)) {
			u.debugf("dropped message: payment time %v with duration %v at ledger time %v", payment.PaymentTime, u.C.MaxRoundDuration, u.LedgerTime)
			return nil
		}
		if payment.PaymentTime.Before(u.C.PaymentTime) {
			u.debugf("dropped message: payment time %v with most recent completed payment time %v", payment.PaymentTime, ch2.PaymentTime)
			return nil
		}
		if u.C.State == AwaitingPaymentMerge {
			if payment.PaymentAmount != u.C.PendingAmountReceived-u.C.PendingAmountSent {
				u.debugf("dropped message: invalid merge payment amount %s", u.C.PendingAmountReceived)
				return nil
			}
		} else {
			u.C.PendingAmountReceived = payment.PaymentAmount
		}
		u.C.setCounterpartySettlementTxes(settleWithGuestTx, settleWithHostTx,
			payment.SenderSettleWithGuestSig, payment.SenderSettleWithHostSig, u.Seed)
		u.C.PendingPaymentTime = payment.PaymentTime
		u.C.RoundNumber++
		return u.transitionTo(PaymentAccepted)

	case PaymentProposed:
		if u.C.RoundNumber != payment.RoundNumber {
			u.debugf("dropped message: payment round %d for channel round %d", payment.RoundNumber, u.C.RoundNumber)
			return nil
		}
		if u.C.PendingAmountSent > payment.PaymentAmount || (u.C.PendingAmountSent == payment.PaymentAmount && u.C.Role == Host) {
			// Create merged payment
			u.C.RoundNumber++
			u.C.PendingAmountSent = u.C.PendingAmountSent - payment.PaymentAmount
			return u.transitionTo(PaymentProposed)
		}
		// Receive merge payment
		u.C.PendingAmountReceived = payment.PaymentAmount
		return u.transitionTo(AwaitingPaymentMerge)
	}
	return nil
}

func (u *Updater) handleCloseMsg(m *Message) error {
	switch u.C.State {
	case Open, PaymentProposed, AwaitingClose: // Accepted states.
	default:
		return errors.Wrap(ErrUnexpectedState, u.C.State)
	}

	var verifyKey keypair.KP
	var err error
	switch u.C.Role {
	case Guest:
		verifyKey, err = keypair.Parse(u.C.EscrowAcct.Address())
		if err != nil {
			return err
		}
	case Host:
		verifyKey, err = keypair.Parse(u.C.GuestAcct.Address())
		if err != nil {
			return err
		}
	}

	coopCloseTx, err := buildCooperativeCloseTx(u.C)
	if err != nil {
		return err
	}

	if err = verifySig(coopCloseTx, verifyKey, m.CloseMsg.CooperativeCloseSig); err != nil {
		return errors.Wrap(err, "coop close tx")
	}

	u.C.CounterpartyCoopCloseSig = m.CloseMsg.CooperativeCloseSig

	return u.transitionTo(AwaitingClose)
}

// bytesToSign gets the representation of the message covered
// by the signature, both for signing and validating purposes.
func (m *Message) bytesToSign() ([]byte, error) {
	msgcopy := *m
	msgcopy.Signature = nil
	return json.Marshal(msgcopy)
}

func (m *Message) signMsg(seed []byte) (*Message, error) {
	if seed == nil {
		return nil, errNoSeed
	}
	bytes, err := m.bytesToSign()
	if err != nil {
		return nil, err
	}
	kp := key.DeriveAccountPrimary(seed)
	m.Signature, err = kp.Sign(bytes)
	return m, err
}
