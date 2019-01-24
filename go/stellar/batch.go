package stellar

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/client/go/protocol/stellar1"
	"github.com/keybase/client/go/stellar/relays"
	"github.com/keybase/client/go/stellar/remote"
	"github.com/keybase/client/go/stellar/stellarcommon"
	"github.com/keybase/stellarnet"
	"github.com/stellar/go/build"
)

const minAmountRelayXLM = "2.01"
const minAmountCreateAccountXLM = "1"

// Batch sends a batch of payments from the user to multiple recipients in
// a time-efficient manner.
func Batch(mctx libkb.MetaContext, walletState *WalletState, arg stellar1.BatchLocalArg) (res stellar1.BatchResultLocal, err error) {
	mctx = mctx.WithLogTag("BATCH=" + arg.BatchID)

	startTime := time.Now()
	res.StartTime = stellar1.ToTimeMs(startTime)
	defer func() {
		if res.EndTime == 0 {
			res.EndTime = stellar1.ToTimeMs(time.Now())
		}
	}()

	// look up sender account
	senderAccountID, senderSeed, err := LookupSenderSeed(mctx)
	if err != nil {
		return res, err
	}

	mctx.CDebugf("Batch sender account ID: %s", senderAccountID)
	mctx.CDebugf("Batch size: %d", len(arg.Payments))

	// prepare the payments
	prepared, err := PrepareBatchPayments(mctx, walletState, senderSeed, arg.Payments)
	if err != nil {
		return res, err
	}

	res.PreparedTime = stellar1.ToTimeMs(time.Now())

	// make a listener that will get payment status updates
	listenerID, listenerCh, err := DefaultLoader(mctx.G()).GetListener()
	if err != nil {
		return res, err
	}
	defer DefaultLoader(mctx.G()).RemoveListener(listenerID)

	resultList := make([]stellar1.BatchPaymentResult, len(prepared))
	waiting := make(map[stellar1.TransactionID]int)

	// submit the payments
	// need to submit tx one at a time, in order
	for i := 0; i < len(prepared); i++ {
		if prepared[i] == nil {
			// this should never happen
			return res, errors.New("batch prepare failed")
		}

		bpResult := stellar1.BatchPaymentResult{
			Username:  prepared[i].Username.String(),
			StartTime: stellar1.ToTimeMs(time.Now()),
		}
		if prepared[i].Error != nil {
			makeResultError(&bpResult, prepared[i].Error)
		} else {
			submitBatchTx(mctx, walletState, senderAccountID, prepared[i], &bpResult)
			if bpResult.Status == stellar1.PaymentStatus_PENDING {
				// add the tx id and the index of this payment to a waiting list
				waiting[bpResult.TxID] = i
			}
		}

		bpResult.StatusDescription = stellar1.PaymentStatusRevMap[bpResult.Status]
		resultList[i] = bpResult
	}

	res.AllSubmittedTime = stellar1.ToTimeMs(time.Now())

	// wait for the payments
	waitingCount := len(waiting)
	mctx.CDebugf("waiting for %d payments to complete", waitingCount)

	timedOut := false
	var chatWaitGroup sync.WaitGroup
	for waitingCount > 0 && !timedOut {
		select {
		case <-time.After(5 * time.Second):
			if time.Since(startTime) > time.Duration(arg.TimeoutSecs)*time.Second {
				mctx.CDebugf("ran out of time waiting for tx status updates (%d remaining)", waitingCount)
				timedOut = true
			}
		case update := <-listenerCh:
			index, ok := waiting[update.TxID]
			if ok {
				mctx.CDebugf("received status update for %s: %s", update.TxID, update.Status)
				resultList[index].Status = update.Status
				resultList[index].StatusDescription = stellar1.PaymentStatusRevMap[update.Status]
				if update.Status != stellar1.PaymentStatus_PENDING {
					waitingCount--
					resultList[index].EndTime = stellar1.ToTimeMs(time.Now())
					delete(waiting, update.TxID)
					mctx.CDebugf("no longer waiting for %s status updates (%d remaining)", update.TxID, waitingCount)
				}
				if update.Status == stellar1.PaymentStatus_COMPLETED {
					chatWaitGroup.Add(1)
					go func(m libkb.MetaContext, recipient string, txID stellar1.TransactionID) {
						if err := chatSendPaymentMessageTo(m, recipient, txID); err != nil {
							m.CDebugf("chatSendPaymentMessageTo %s (%s): error: %s", recipient, txID, err)
						} else {
							m.CDebugf("chatSendPaymentMessageTo %s (%s): success", recipient, txID)
						}

						chatWaitGroup.Done()
					}(mctx.WithCtx(context.Background()), resultList[index].Username, update.TxID)
				}
			}
		}
	}

	mctx.CDebugf("done waiting for payments to complete")

	mctx.CDebugf("waiting for chat messages to finish sending")
	chatWaitGroup.Wait()
	mctx.CDebugf("done waiting for chat messages to finish sending")

	res.Payments = resultList
	res.EndTime = stellar1.ToTimeMs(time.Now())
	calculateStats(&res)

	return res, nil
}

// PrepareBatchPayments prepares a list of payments to be submitted.
// Each payment is prepared concurrently.
// (this is an exposed function to make testing from outside this package easier)
func PrepareBatchPayments(mctx libkb.MetaContext, walletState *WalletState, senderSeed stellarnet.SeedStr, payments []stellar1.BatchPaymentArg) ([]*MiniPrepared, error) {
	mctx.CDebugf("preparing %d batch payments", len(payments))

	prepared := make(chan *MiniPrepared)

	sp := NewSeqnoProvider(mctx, walletState)
	for _, payment := range payments {
		go func(p stellar1.BatchPaymentArg) {
			prepared <- prepareBatchPayment(mctx, walletState, sp, senderSeed, p)
		}(payment)
	}

	// prepared chan could be out of order, so sort by seqno
	preparedList := make([]*MiniPrepared, len(payments))
	for i := 0; i < len(payments); i++ {
		preparedList[i] = <-prepared
	}
	sort.Slice(preparedList, func(a, b int) bool { return preparedList[a].Seqno < preparedList[b].Seqno })

	return preparedList, nil
}

func prepareBatchPayment(mctx libkb.MetaContext, remoter remote.Remoter, sp build.SequenceProvider, senderSeed stellarnet.SeedStr, payment stellar1.BatchPaymentArg) *MiniPrepared {
	recipient, err := LookupRecipient(mctx, stellarcommon.RecipientInput(payment.Recipient), false /* isCLI for identify purposes */)
	if err != nil {
		mctx.CDebugf("LookupRecipient error: %s", err)
		return &MiniPrepared{
			Username: libkb.NewNormalizedUsername(payment.Recipient),
			Error:    errors.New("error looking up recipient"),
		}
	}

	if recipient.AccountID == nil {
		return prepareBatchPaymentRelay(mctx, remoter, sp, senderSeed, payment, recipient)
	}
	return prepareBatchPaymentDirect(mctx, remoter, sp, senderSeed, payment, recipient)
}

func prepareBatchPaymentDirect(mctx libkb.MetaContext, remoter remote.Remoter, sp build.SequenceProvider, senderSeed stellarnet.SeedStr, payment stellar1.BatchPaymentArg, recipient stellarcommon.Recipient) *MiniPrepared {
	result := &MiniPrepared{Username: libkb.NewNormalizedUsername(payment.Recipient)}
	funded, err := isAccountFunded(mctx.Ctx(), remoter, stellar1.AccountID(recipient.AccountID.String()))
	if err != nil {
		result.Error = err
		return result
	}

	if !funded {
		if isAmountLessThanMin(payment.Amount, minAmountCreateAccountXLM) {
			result.Error = fmt.Errorf("you must send at least %s XLM to fund the account for %s", minAmountCreateAccountXLM, payment.Recipient)
			return result
		}

	}

	result.Direct = &stellar1.PaymentDirectPost{
		FromDeviceID: mctx.G().ActiveDevice.DeviceID(),
		To:           &recipient.User.UV,
		QuickReturn:  true,
	}

	var signResult stellarnet.SignResult
	if funded {
		signResult, err = stellarnet.PaymentXLMTransaction(senderSeed, *recipient.AccountID, payment.Amount, "", sp, nil)
	} else {
		signResult, err = stellarnet.CreateAccountXLMTransaction(senderSeed, *recipient.AccountID, payment.Amount, "", sp, nil)
	}
	if err != nil {
		result.Error = err
		return result
	}

	if len(payment.Message) > 0 {
		noteClear := stellar1.NoteContents{
			Note:      payment.Message,
			StellarID: stellar1.TransactionID(signResult.TxHash),
		}
		var recipientUv *keybase1.UserVersion
		if recipient.User != nil {
			recipientUv = &recipient.User.UV
		}
		result.Direct.NoteB64, err = NoteEncryptB64(mctx, noteClear, recipientUv)
		if err != nil {
			result.Error = fmt.Errorf("error encrypting note: %v", err)
			return result
		}
	}

	result.Direct.SignedTransaction = signResult.Signed
	result.Seqno = signResult.Seqno
	result.TxID = stellar1.TransactionID(signResult.TxHash)

	return result
}

func prepareBatchPaymentRelay(mctx libkb.MetaContext, remoter remote.Remoter, sp build.SequenceProvider, senderSeed stellarnet.SeedStr, payment stellar1.BatchPaymentArg, recipient stellarcommon.Recipient) *MiniPrepared {
	result := &MiniPrepared{Username: libkb.NewNormalizedUsername(payment.Recipient)}

	if isAmountLessThanMin(payment.Amount, minAmountRelayXLM) {
		result.Error = fmt.Errorf("you must send at least %s XLM to fund the account for %s", minAmountRelayXLM, payment.Recipient)
		return result
	}

	appKey, teamID, err := relays.GetKey(mctx, recipient)
	if err != nil {
		result.Error = err
		return result
	}

	relay, err := relays.Create(relays.Input{
		From:          stellar1.SecretKey(senderSeed),
		AmountXLM:     payment.Amount,
		Note:          payment.Message,
		EncryptFor:    appKey,
		SeqnoProvider: sp,
		Timebounds:    nil,
	})
	if err != nil {
		result.Error = err
		return result
	}

	post := stellar1.PaymentRelayPost{
		FromDeviceID:      mctx.ActiveDevice().DeviceID(),
		ToAssertion:       string(recipient.Input),
		RelayAccount:      relay.RelayAccountID,
		TeamID:            teamID,
		BoxB64:            relay.EncryptedB64,
		SignedTransaction: relay.FundTx.Signed,
		QuickReturn:       true,
	}
	if recipient.User != nil {
		post.To = &recipient.User.UV
	}

	result.Relay = &post
	result.Seqno = relay.FundTx.Seqno
	result.TxID = stellar1.TransactionID(relay.FundTx.TxHash)

	return result
}

func calculateStats(res *stellar1.BatchResultLocal) {
	res.OverallDurationMs = res.EndTime - res.StartTime
	res.PrepareDurationMs = res.PreparedTime - res.StartTime
	res.SubmitDurationMs = res.AllSubmittedTime - res.PreparedTime
	res.WaitDurationMs = res.EndTime - res.AllSubmittedTime

	var durationTotal stellar1.TimeMs
	var durationSuccess stellar1.TimeMs
	var durationError stellar1.TimeMs
	var countDone int64

	for _, p := range res.Payments {
		duration := p.EndTime - p.StartTime
		durationTotal += duration
		switch p.Status {
		case stellar1.PaymentStatus_COMPLETED:
			countDone++
			res.CountSuccess++
			durationSuccess += duration
		case stellar1.PaymentStatus_PENDING:
			res.CountPending++
		default:
			// error
			countDone++
			res.CountError++
			durationError += duration
		}
	}

	if countDone > 0 {
		res.AvgDurationMs = stellar1.TimeMs(int64(durationTotal) / countDone)
	}

	if res.CountSuccess > 0 {
		res.AvgSuccessDurationMs = stellar1.TimeMs(int64(durationSuccess) / int64(res.CountSuccess))
	}

	if res.CountError > 0 {
		res.AvgErrorDurationMs = stellar1.TimeMs(int64(durationError) / int64(res.CountError))
	}
}

func makeResultError(res *stellar1.BatchPaymentResult, err error) {
	res.EndTime = stellar1.ToTimeMs(time.Now())
	res.Error = &stellar1.BatchPaymentError{Message: err.Error()}
	res.Status = stellar1.PaymentStatus_ERROR
}

func submitBatchTx(mctx libkb.MetaContext, walletState *WalletState, senderAccountID stellar1.AccountID, prepared *MiniPrepared, bpResult *stellar1.BatchPaymentResult) {
	mctx.CDebugf("submitting batch payment seqno %d", prepared.Seqno)

	err := walletState.AddPendingTx(mctx.Ctx(), senderAccountID, prepared.TxID, prepared.Seqno)
	if err != nil {
		// it's ok to keep going here
		mctx.CDebugf("error calling AddPendingTx: %s", err)
	}

	var submitRes stellar1.PaymentResult
	switch {
	case prepared.Direct != nil:
		submitRes, err = walletState.SubmitPayment(mctx.Ctx(), *prepared.Direct)
	case prepared.Relay != nil:
		submitRes, err = walletState.SubmitRelayPayment(mctx.Ctx(), *prepared.Relay)
	default:
		err = errors.New("no prepared direct or relay payment")
	}

	bpResult.SubmittedTime = stellar1.ToTimeMs(time.Now())

	if err != nil {
		mctx.CDebugf("error submitting batch payment seqno %d, txid %s: %s", prepared.Seqno, prepared.TxID, err)
		makeResultError(bpResult, err)
		return
	}

	bpResult.TxID = submitRes.StellarID
	if submitRes.Pending {
		bpResult.Status = stellar1.PaymentStatus_PENDING
	} else {
		bpResult.Status = stellar1.PaymentStatus_COMPLETED
		bpResult.EndTime = stellar1.ToTimeMs(time.Now())
	}
}
