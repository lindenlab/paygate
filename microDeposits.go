// Copyright 2018 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package paygate

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	accounts "github.com/moov-io/accounts/client"
	"github.com/moov-io/ach"
	"github.com/moov-io/base"
	"github.com/moov-io/base/admin"
	moovhttp "github.com/moov-io/base/http"
	"github.com/moov-io/paygate/internal/util"

	"github.com/go-kit/kit/log"
)

// ODFIAccount represents the depository account micro-deposts are debited from
type ODFIAccount struct {
	accountNumber string
	routingNumber string
	accountType   AccountType

	client AccountsClient

	mu        sync.Mutex
	accountID string
}

func NewODFIAccount(accountsClient AccountsClient, accountNumber string, routingNumber string, accountType AccountType) *ODFIAccount {
	return &ODFIAccount{
		client:        accountsClient,
		accountNumber: accountNumber,
		routingNumber: routingNumber,
		accountType:   accountType,
	}
}

func (a *ODFIAccount) getID(requestID, userID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accountID != "" {
		return a.accountID, nil
	}
	if a.client == nil {
		return "", errors.New("ODFIAccount: nil AccountsClient")
	}

	// Otherwise, make our Accounts HTTP call and grab the ID
	dep := &Depository{
		AccountNumber: a.accountNumber,
		RoutingNumber: a.routingNumber,
		Type:          a.accountType,
	}
	acct, err := a.client.SearchAccounts(requestID, userID, dep)
	if err != nil || (acct == nil || acct.ID == "") {
		return "", fmt.Errorf("ODFIAccount: problem getting accountID: %v", err)
	}
	a.accountID = acct.ID // record account ID for calls later on
	return a.accountID, nil
}

func (a *ODFIAccount) metadata() (*Originator, *Depository) {
	orig := &Originator{
		ID:                "odfi", // TODO(adam): make this NOT querable via db.
		DefaultDepository: DepositoryID("odfi"),
		Identification:    util.Or(os.Getenv("ODFI_IDENTIFICATION"), "001"),
		Metadata:          "Moov - paygate micro-deposits",
	}
	dep := &Depository{
		ID:            DepositoryID("odfi"),
		BankName:      util.Or(os.Getenv("ODFI_BANK_NAME"), "Moov, Inc"),
		Holder:        util.Or(os.Getenv("ODFI_HOLDER"), "Moov, Inc"),
		HolderType:    Individual,
		Type:          a.accountType,
		RoutingNumber: a.routingNumber,
		AccountNumber: a.accountNumber,
		Status:        DepositoryVerified,
	}
	return orig, dep
}

type microDeposit struct {
	amount Amount
	fileID string
}

func (m microDeposit) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Amount Amount `json:"amount"`
	}{
		m.amount,
	})
}

func microDepositAmounts() ([]Amount, int) {
	rand := func() int {
		n, _ := rand.Int(rand.Reader, big.NewInt(49)) // rand.Int returns [0, N) and we want a range of $0.01 to $0.50
		return int(n.Int64()) + 1
	}
	// generate two amounts and a third that's the sum
	n1, n2 := rand(), rand()
	a1, _ := NewAmount("USD", fmt.Sprintf("0.%02d", n1)) // pad 1 to '01'
	a2, _ := NewAmount("USD", fmt.Sprintf("0.%02d", n2))
	return []Amount{*a1, *a2}, n1 + n2
}

// initiateMicroDeposits will write micro deposits into the underlying database and kick off the ACH transfer(s).
//
func (r *DepositoryRouter) initiateMicroDeposits() http.HandlerFunc {
	return func(w http.ResponseWriter, httpReq *http.Request) {
		w, err := wrapResponseWriter(r.logger, w, httpReq)
		if err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		requestID := moovhttp.GetRequestID(httpReq)

		id, userID := getDepositoryID(httpReq), moovhttp.GetUserID(httpReq)
		if id == "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			// 404 - A depository with the specified ID was not found.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": "depository not found"}`))
			return
		}

		// Check the depository status and confirm it belongs to the user
		dep, err := r.depositoryRepo.getUserDepository(id, userID)
		if err != nil {
			r.logger.Log("microDeposits", err, "requestID", requestID, "userID", userID)
			moovhttp.Problem(w, err)
			return
		}
		if dep == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if dep.Status != DepositoryUnverified {
			err = fmt.Errorf("depository %s in bogus status %s", dep.ID, dep.Status)
			r.logger.Log("microDeposits", err, "requestID", requestID, "userID", userID)
			moovhttp.Problem(w, err)
			return
		}

		// Our Depository needs to be Verified so let's submit some micro deposits to it.
		amounts, sum := microDepositAmounts()
		microDeposits, err := r.submitMicroDeposits(userID, requestID, amounts, sum, dep)
		if err != nil {
			err = fmt.Errorf("problem submitting micro-deposits: %v", err)
			r.logger.Log("microDeposits", err, "requestID", requestID, "userID", userID)
			moovhttp.Problem(w, err)
			return
		}
		r.logger.Log("microDeposits", fmt.Sprintf("submitted %d micro-deposits for depository=%s", len(microDeposits), dep.ID), "requestID", requestID, "userID", userID)

		// Write micro deposits into our db
		if err := r.depositoryRepo.initiateMicroDeposits(id, userID, microDeposits); err != nil {
			r.logger.Log("microDeposits", err, "requestID", requestID, "userID", userID)
			moovhttp.Problem(w, err)
			return
		}
		r.logger.Log("microDeposits", fmt.Sprintf("stored micro-deposits for depository=%s", dep.ID), "requestID", requestID, "userID", userID)

		w.WriteHeader(http.StatusCreated) // 201 - Micro deposits initiated
		w.Write([]byte("{}"))
	}
}

func postMicroDepositTransaction(logger log.Logger, client AccountsClient, accountID, userID string, lines []transactionLine, requestID string) (*accounts.Transaction, error) {
	var transaction *accounts.Transaction
	var err error
	for i := 0; i < 3; i++ {
		transaction, err = client.PostTransaction(requestID, userID, lines)
		if err == nil {
			break // quit after successful call
		}
	}
	if err != nil {
		return nil, fmt.Errorf("error creating transaction for transfer user=%s: %v", userID, err)
	}
	logger.Log("transfers", fmt.Sprintf("created transaction=%s for user=%s", transaction.ID, userID), "requestID", requestID)
	return transaction, nil
}

func postMicroDepositTransactions(logger log.Logger, ODFIAccount *ODFIAccount, client AccountsClient, userID string, dep *Depository, amounts []Amount, sum int, requestID string) ([]*accounts.Transaction, error) {
	if len(amounts) != 2 {
		return nil, fmt.Errorf("postMicroDepositTransactions: unexpected %d Amounts", len(amounts))
	}
	acct, err := client.SearchAccounts(requestID, userID, dep)
	if err != nil || acct == nil {
		return nil, fmt.Errorf("error reading account user=%s depository=%s: %v", userID, dep.ID, err)
	}
	ODFIAccountID, err := ODFIAccount.getID(requestID, userID)
	if err != nil {
		return nil, fmt.Errorf("posting micro-deposits: %v", err)
	}

	// Submit all micro-deposits
	var transactions []*accounts.Transaction
	for i := range amounts {
		lines := []transactionLine{
			{AccountID: acct.ID, Purpose: "ACHCredit", Amount: int32(amounts[i].Int())},
			{AccountID: ODFIAccountID, Purpose: "ACHDebit", Amount: int32(amounts[i].Int())},
		}
		tx, err := postMicroDepositTransaction(logger, client, acct.ID, userID, lines, requestID)
		if err != nil {
			return nil, err // we retried and failed, so just exit early
		}
		transactions = append(transactions, tx)
	}
	// submit the reversal of our micro-deposits
	lines := []transactionLine{
		{AccountID: acct.ID, Purpose: "ACHDebit", Amount: int32(sum)},
		{AccountID: ODFIAccountID, Purpose: "ACHCredit", Amount: int32(sum)},
	}
	tx, err := postMicroDepositTransaction(logger, client, acct.ID, userID, lines, requestID)
	if err != nil {
		return nil, fmt.Errorf("postMicroDepositTransaction: on sum transaction post: %v", err)
	}
	transactions = append(transactions, tx)
	return transactions, nil
}

// submitMicroDeposits will create ACH files to process multiple micro-deposit transfers to validate a Depository.
// The Originator used belongs to the ODFI (or Moov in tests).
//
// The steps needed are:
// - Grab related transfer objects for the user
// - Create several Transfers and create their ACH files (then validate)
// - Write micro-deposits to SQL table (used in /confirm endpoint)
//
// submitMicroDeposits assumes there are 2 amounts to credit and a third to debit.
func (r *DepositoryRouter) submitMicroDeposits(userID string, requestID string, amounts []Amount, sum int, dep *Depository) ([]microDeposit, error) {
	odfiOriginator, odfiDepository := r.odfiAccount.metadata()

	// TODO(adam): reject if user has been failed too much verifying this Depository -- w.WriteHeader(http.StatusConflict)

	var microDeposits []microDeposit
	for i := range amounts {
		req := &transferRequest{
			Amount:                 amounts[i],
			Originator:             odfiOriginator.ID, // e.g. Moov, Inc
			OriginatorDepository:   odfiDepository.ID,
			Description:            fmt.Sprintf("%s micro-deposit verification", odfiDepository.BankName),
			StandardEntryClassCode: ach.PPD,
		}
		// micro-deposits must balance, the 3rd amount is the other two's sum
		if i == 0 || i == 1 {
			req.Type = PushTransfer
		} else {
			req.Type = PullTransfer
		}

		// The Receiver and ReceiverDepository are the Depository that needs approval.
		req.Receiver = ReceiverID(fmt.Sprintf("%s-micro-deposit-verify", base.ID()))
		req.ReceiverDepository = dep.ID
		rec := &Receiver{
			ID:       req.Receiver,
			Status:   ReceiverVerified, // Something to pass constructACHFile validation logic
			Metadata: dep.Holder,       // Depository holder is getting the micro deposit
		}

		// Convert to Transfer object
		xfer := req.asTransfer(string(rec.ID))

		// Build and submit the file (with micro-deposits) to moov's ACH service
		idempotencyKey := base.ID()
		file, err := constructACHFile(string(xfer.ID), idempotencyKey, userID, xfer, rec, dep, odfiOriginator, odfiDepository)
		if err != nil {
			err = fmt.Errorf("problem constructing ACH file for userID=%s: %v", userID, err)
			r.logger.Log("microDeposits", err, "requestID", requestID, "userID", userID)
			return nil, err
		}
		// We need to withdraw the micro-deposit from the remote account. To do this simply debit that account by adding another EntryDetail
		addMicroDepositReversal(file)

		// Submit the ACH file against moov's ACH service.
		fileID, err := r.achClient.CreateFile(idempotencyKey, file)
		if err != nil {
			err = fmt.Errorf("problem creating ACH file for userID=%s: %v", userID, err)
			r.logger.Log("microDeposits", err, "requestID", requestID, "userID", userID)
			return nil, err
		}
		if err := checkACHFile(r.logger, r.achClient, fileID, userID); err != nil {
			return nil, err
		}
		r.logger.Log("microDeposits", fmt.Sprintf("created ACH file=%s depository=%s", xfer.ID, dep.ID), "requestID", requestID, "userID", userID)

		// Store the Transfer creation as an event
		if err := writeTransferEvent(userID, req, r.eventRepo); err != nil {
			return nil, fmt.Errorf("userID=%s problem writing micro-deposit transfer event: %v", userID, err)
		}

		microDeposits = append(microDeposits, microDeposit{
			amount: amounts[i],
			fileID: fileID,
		})
	}
	// Post the transaction against Accounts only if it's enabled (flagged via nil AccountsClient)
	if r.accountsClient != nil {
		transactions, err := postMicroDepositTransactions(r.logger, r.odfiAccount, r.accountsClient, userID, dep, amounts, sum, requestID)
		if err != nil {
			return microDeposits, fmt.Errorf("submitMicroDeposits: error posting to Accounts: %v", err)
		}
		r.logger.Log("microDeposits", fmt.Sprintf("created %d transactions for user=%s micro-deposits", len(transactions), userID), "requestID", requestID)
	}
	return microDeposits, nil
}

func addMicroDepositReversal(file *ach.File) {
	if file == nil || len(file.Batches) != 1 || len(file.Batches[0].GetEntries()) != 1 {
		return // invalid file
	}

	// We need to adjust ServiceClassCode as this batch has a debit and credit now
	bh := file.Batches[0].GetHeader()
	bh.ServiceClassCode = ach.MixedDebitsAndCredits
	file.Batches[0].SetHeader(bh)

	// Copy the EntryDetail and replace TransactionCode
	ed := *file.Batches[0].GetEntries()[0] // copy the existing EntryDetail
	ed.ID = base.ID()[:8]
	// TransactionCodes seem to follow a simple pattern:
	//  37 SavingsDebit -> 32 SavingsCredit
	//  27 CheckingDebit -> 22 CheckingCredit
	ed.TransactionCode -= 5

	// increment trace number
	if n, _ := strconv.Atoi(ed.TraceNumber); n > 0 {
		ed.TraceNumber = strconv.Itoa(n + 1)
	}

	// append our new EntryDetail
	file.Batches[0].AddEntry(&ed)
}

type confirmDepositoryRequest struct {
	Amounts []string `json:"amounts"`
}

// confirmMicroDeposits checks our database for a depository's micro deposits (used to validate the user owns the Depository)
// and if successful changes the Depository status to DepositoryVerified.
//
// TODO(adam): Should we allow a Depository to be confirmed before the micro-deposit ACH file is
// upload? Technically there's really no way for an end-user to see them before posting, however
// out demo and tests can lookup in Accounts right away and quickly verify the Depository.
func (r *DepositoryRouter) confirmMicroDeposits() http.HandlerFunc {
	return func(w http.ResponseWriter, httpReq *http.Request) {
		w, err := wrapResponseWriter(r.logger, w, httpReq)
		if err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		id, userID := getDepositoryID(httpReq), moovhttp.GetUserID(httpReq)
		if id == "" {
			// 404 - A depository with the specified ID was not found.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": "depository not found"}`))
			return
		}

		// Check the depository status and confirm it belongs to the user
		dep, err := r.depositoryRepo.getUserDepository(id, userID)
		if err != nil {
			r.logger.Log("confirmMicroDeposits", err, "userID", userID)
			moovhttp.Problem(w, err)
			return
		}
		if dep.Status != DepositoryUnverified {
			err = fmt.Errorf("depository %s in bogus status %s", dep.ID, dep.Status)
			r.logger.Log("confirmMicroDeposits", err, "userID", userID)
			moovhttp.Problem(w, err)
			return
		}

		// TODO(adam): if we've failed too many times return '409 - Too many attempts'

		// Read amounts from request JSON
		var req confirmDepositoryRequest
		rr := io.LimitReader(httpReq.Body, maxReadBytes)
		if err := json.NewDecoder(rr).Decode(&req); err != nil {
			r.logger.Log("confirmDepositoryRequest", fmt.Sprintf("problem reading request: %v", err), "userID", userID)
			moovhttp.Problem(w, err)
			return
		}

		var amounts []Amount
		for i := range req.Amounts {
			amt := &Amount{}
			if err := amt.FromString(req.Amounts[i]); err != nil {
				continue
			}
			amounts = append(amounts, *amt)
		}
		if len(amounts) == 0 {
			r.logger.Log("confirmMicroDeposits", "no micro-deposit amounts found", "userID", userID)
			// 400 - Invalid Amounts
			moovhttp.Problem(w, errors.New("invalid amounts, found none"))
			return
		}
		if err := r.depositoryRepo.confirmMicroDeposits(id, userID, amounts); err != nil {
			r.logger.Log("confirmMicroDeposits", fmt.Sprintf("problem confirming micro-deposits: %v", err), "userID", userID)
			moovhttp.Problem(w, err)
			return
		}

		// Update Depository status
		if err := markDepositoryVerified(r.depositoryRepo, id, userID); err != nil {
			r.logger.Log("confirmMicroDeposits", fmt.Sprintf("problem marking depository as Verified: %v", err), "userID", userID)
			return
		}

		// 200 - Micro deposits verified
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
	}
}

func AddMicroDepositAdminRoutes(logger log.Logger, svc *admin.Server, depRepo DepositoryRepository) {
	svc.AddHandler("/depositories/{depositoryId}/micro-deposits", getMicroDeposits(logger, depRepo))
}

// getMicroDeposits is an http.HandlerFunc for paygate's admin server to return micro-deposits for a given Depository
//
// This endpoint should not be exposed on the business http port as it would allow anyone to automatically verify a Depository
// without micro-deposits.
func getMicroDeposits(logger log.Logger, depositoryRepo DepositoryRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w = wrap(logger, w, r)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if r.Method != "GET" {
			moovhttp.Problem(w, fmt.Errorf("unsupported HTTP verb: %s", r.Method))
			return
		}

		id, userID := getDepositoryID(r), moovhttp.GetUserID(r)
		requestID := moovhttp.GetRequestID(r)
		if id == "" {
			// 404 - A depository with the specified ID was not found.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": "depository not found"}`))
			return
		}

		microDeposits, err := depositoryRepo.getMicroDeposits(id)
		if err != nil {
			logger.Log("microDeposits", fmt.Sprintf("admin: problem reading micro-deposits for depository=%s: %v", id, err), "requestID", requestID, "userID", userID)
			moovhttp.Problem(w, err)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(microDeposits)
	}
}

// getMicroDeposits will retrieve the micro deposits for a given depository. This endpoint is designed for paygate's admin endpoints.
// If an amount does not parse it will be discardded silently.
func (r *SQLDepositoryRepo) getMicroDeposits(id DepositoryID) ([]microDeposit, error) {
	query := `select amount, file_id from micro_deposits where depository_id = ?`
	stmt, err := r.db.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	rows, err := stmt.Query(id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return accumulateMicroDeposits(rows)
}

// getMicroDepositsForUser will retrieve the micro deposits for a given depository. If an amount does not parse it will be discardded silently.
func (r *SQLDepositoryRepo) getMicroDepositsForUser(id DepositoryID, userID string) ([]microDeposit, error) {
	query := `select amount, file_id from micro_deposits where user_id = ? and depository_id = ? and deleted_at is null`
	stmt, err := r.db.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	rows, err := stmt.Query(userID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return accumulateMicroDeposits(rows)
}

func accumulateMicroDeposits(rows *sql.Rows) ([]microDeposit, error) {
	var microDeposits []microDeposit
	for rows.Next() {
		var fileID string
		var value string
		if err := rows.Scan(&value, &fileID); err != nil {
			continue
		}

		amt := &Amount{}
		if err := amt.FromString(value); err != nil {
			continue
		}
		microDeposits = append(microDeposits, microDeposit{
			amount: *amt,
			fileID: fileID,
		})
	}
	return microDeposits, rows.Err()
}

// initiateMicroDeposits will save the provided []Amount into our database. If amounts have already been saved then
// no new amounts will be added.
func (r *SQLDepositoryRepo) initiateMicroDeposits(id DepositoryID, userID string, microDeposits []microDeposit) error {
	existing, err := r.getMicroDepositsForUser(id, userID)
	if err != nil || len(existing) > 0 {
		return fmt.Errorf("not initializing more micro deposits, already have %d or got error=%v", len(existing), err)
	}

	// write amounts
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}

	now, query := time.Now(), `insert into micro_deposits (depository_id, user_id, amount, file_id, created_at) values (?, ?, ?, ?, ?)`
	stmt, err := tx.Prepare(query)
	if err != nil {
		return fmt.Errorf("initiateMicroDeposits: prepare error=%v rollback=%v", err, tx.Rollback())
	}
	defer stmt.Close()

	for i := range microDeposits {
		_, err = stmt.Exec(id, userID, microDeposits[i].amount.String(), microDeposits[i].fileID, now)
		if err != nil {
			return fmt.Errorf("initiateMicroDeposits: scan error=%v rollback=%v", err, tx.Rollback())
		}
	}

	return tx.Commit()
}

// confirmMicroDeposits will compare the provided guessAmounts against what's been persisted for a user. If the amounts do not match
// or there are a mismatched amount the call will return a non-nil error.
func (r *SQLDepositoryRepo) confirmMicroDeposits(id DepositoryID, userID string, guessAmounts []Amount) error {
	microDeposits, err := r.getMicroDepositsForUser(id, userID)
	if err != nil {
		return fmt.Errorf("unable to confirm micro deposits, got error=%v", err)
	}
	if len(microDeposits) == 0 {
		return errors.New("unable to confirm micro deposits, got 0 micro deposits")
	}

	// Check amounts, all must match
	if len(guessAmounts) != len(microDeposits) || len(guessAmounts) == 0 {
		return fmt.Errorf("incorrect amount of guesses, got %d", len(guessAmounts)) // don't share len(microDeposits), that's an info leak
	}

	found := 0
	for i := range microDeposits {
		for k := range guessAmounts {
			if microDeposits[i].amount.Equal(guessAmounts[k]) {
				found += 1
				break
			}
		}
	}

	if found != len(microDeposits) {
		return errors.New("incorrect micro deposit guesses")
	}

	return nil
}

// getMicroDepositCursor returns a microDepositCursor for iterating through micro-deposits in ascending order (by CreatedAt)
// beginning at the start of the current day.
func (r *SQLDepositoryRepo) getMicroDepositCursor(batchSize int) *microDepositCursor {
	now := time.Now()
	return &microDepositCursor{
		batchSize: batchSize,
		depRepo:   r,
		newerThan: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
	}
}

// TODO(adam): microDepositCursor (similar to transferCursor for ACH file merging and uploads)
// micro_deposits(depository_id, user_id, amount, file_id, created_at, deleted_at)`
type microDepositCursor struct {
	batchSize int

	depRepo *SQLDepositoryRepo

	// newerThan represents the minimum (oldest) created_at value to return in the batch.
	// The value starts at today's first instant and progresses towards time.Now() with each
	// batch by being set to the batch's newest time.
	newerThan time.Time
}

type uploadableMicroDeposit struct {
	depositoryID string
	userID       string
	amount       *Amount
	fileID       string
	createdAt    time.Time
}

// Next returns a slice of micro-deposit objects from the current day. Next should be called to process
// all objects for a given day in batches.
func (cur *microDepositCursor) Next() ([]uploadableMicroDeposit, error) {
	query := `select depository_id, user_id, amount, file_id, created_at from micro_deposits where deleted_at is null and merged_filename is null and created_at > ? order by created_at asc limit ?`
	stmt, err := cur.depRepo.db.Prepare(query)
	if err != nil {
		return nil, fmt.Errorf("microDepositCursor.Next: prepare: %v", err)
	}
	defer stmt.Close()

	rows, err := stmt.Query(cur.newerThan, cur.batchSize)
	if err != nil {
		return nil, fmt.Errorf("microDepositCursor.Next: query: %v", err)
	}
	defer rows.Close()

	max := cur.newerThan
	var microDeposits []uploadableMicroDeposit
	for rows.Next() {
		var m uploadableMicroDeposit
		var amt string
		if err := rows.Scan(&m.depositoryID, &m.userID, &amt, &m.fileID, &m.createdAt); err != nil {
			return nil, fmt.Errorf("transferCursor.Next: scan: %v", err)
		}
		var amount Amount
		if err := amount.FromString(amt); err != nil {
			return nil, fmt.Errorf("transferCursor.Next: %s Amount from string: %v", amt, err)
		}
		m.amount = &amount
		if m.createdAt.After(max) {
			max = m.createdAt // advance to latest timestamp
		}
		microDeposits = append(microDeposits, m)
	}
	cur.newerThan = max
	return microDeposits, rows.Err()
}

// markMicroDepositAsMerged will set the merged_filename on micro-deposits so they aren't merged into multiple files
// and the file uploaded to the Federal Reserve can be tracked.
func (r *SQLDepositoryRepo) markMicroDepositAsMerged(filename string, mc uploadableMicroDeposit) error {
	query := `update micro_deposits set merged_filename = ?
where depository_id = ? and file_id = ? and amount = ? and (merged_filename is null or merged_filename = '') and deleted_at is null`
	stmt, err := r.db.Prepare(query)
	if err != nil {
		return fmt.Errorf("markMicroDepositAsMerged: filename=%s: %v", filename, err)
	}
	defer stmt.Close()

	_, err = stmt.Exec(filename, mc.depositoryID, mc.fileID, mc.amount.String())
	return err
}
