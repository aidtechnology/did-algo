package internal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/transaction"
	"github.com/algorand/go-algorand-sdk/v2/types"
	"go.bryk.io/pkg/did"
	xlog "go.bryk.io/pkg/log"
)

// ClientSettings defines the configuration options available when
// interacting with an AlgoDID network agent.
type ClientSettings struct {
	Active   string            `json:"active" yaml:"active" mapstructure:"active"`
	Profiles []*NetworkProfile `json:"profiles" yaml:"profiles" mapstructure:"profiles"`
}

// NetworkProfile defines the configuration options to connect to a
// specific AlgoDID network agent.
type NetworkProfile struct {
	// Profile name.
	Name string `json:"name" yaml:"name" mapstructure:"name"`

	// Algod node address.
	Node string `json:"node" yaml:"node" mapstructure:"node"`

	// Algod node access token.
	NodeToken string `json:"node_token,omitempty" yaml:"node_token,omitempty" mapstructure:"node_token"`

	// Application ID for the AlgoDID storage smart contract.
	AppID uint `json:"app_id" yaml:"app_id" mapstructure:"app_id"`

	// Storage contract provider server, if any.
	StoreProvider string `json:"store_provider,omitempty" yaml:"store_provider,omitempty" mapstructure:"store_provider"`
}

// AlgoClient provides a simplified interface to interact with the
// Algorand network.
type AlgoClient struct {
	np    *NetworkProfile
	log   xlog.Logger
	httpC *http.Client
	algod *algod.Client
}

// NewAlgoClient creates a new instance of the AlgoClient.
func NewAlgoClient(profile *NetworkProfile, log xlog.Logger) (*AlgoClient, error) {
	if profile == nil {
		return nil, fmt.Errorf("no network profile provided")
	}
	client, err := algod.MakeClient(profile.Node, profile.NodeToken)
	if err != nil {
		return nil, err
	}
	return &AlgoClient{
		np:    profile,
		log:   log,
		algod: client,
		httpC: &http.Client{},
	}, nil
}

// StorageAppID returns the application ID for the AlgoDID storage.
func (c *AlgoClient) StorageAppID() uint {
	return c.np.AppID
}

// SuggestedParams returns the suggested transaction parameters.
func (c *AlgoClient) SuggestedParams() (types.SuggestedParams, error) {
	return c.algod.SuggestedParams().Do(context.TODO())
}

// SubmitTx sends a raw signed transaction to the network.
func (c *AlgoClient) SubmitTx(stx []byte) (string, error) {
	return c.algod.SendRawTransaction(stx).Do(context.TODO())
}

// AccountInformation returns the account information for the given address.
func (c *AlgoClient) AccountInformation(address string) (models.Account, error) {
	return c.algod.AccountInformation(address).Do(context.TODO())
}

// Ready returns true if the network is available.
func (c *AlgoClient) Ready() bool {
	return c.algod.HealthCheck().Do(context.TODO()) == nil
}

// DeployContract creates a new instance of the AlgoDID storage smart contract
// on the network.
func (c *AlgoClient) DeployContract(sender *crypto.Account) (uint64, error) {
	contract := loadContract()
	signer := transaction.BasicAccountTransactionSigner{Account: *sender}
	return createApp(c.algod, contract, signer.Account.Address, signer)
}

// PublishDID sends a new DID document to the network
// fot on-chain storage.
func (c *AlgoClient) PublishDID(id *did.Identifier, sender *crypto.Account) error {
	c.log.WithFields(map[string]interface{}{
		"did": id.String(),
	}).Info("publishing DID document")
	contract := loadContract()
	signer := transaction.BasicAccountTransactionSigner{Account: *sender}
	doc, _ := json.Marshal(id.Document(true))
	pub, appID, err := parseSubjectString(id.Subject())
	if err != nil {
		return err
	}
	if c.np.StoreProvider != "" {
		return c.submitToProvider(pub, appID, http.MethodPost, doc)
	}
	return publishDID(c.algod, appID, contract, sender.Address, signer, doc, pub)
}

// DeleteDID removes a DID document from the network.
func (c *AlgoClient) DeleteDID(id *did.Identifier, sender *crypto.Account) error {
	c.log.WithFields(map[string]interface{}{
		"did": id.String(),
	}).Info("deleting DID document")
	contract := loadContract()
	signer := transaction.BasicAccountTransactionSigner{Account: *sender}
	pub, appID, err := parseSubjectString(id.Subject())
	if err != nil {
		return err
	}
	if c.np.StoreProvider != "" {
		return c.submitToProvider(pub, appID, http.MethodDelete, nil)
	}
	return deleteDID(appID, pub, sender.Address, c.algod, contract, signer)
}

// Resolve retrieves a DID document from the network.
func (c *AlgoClient) Resolve(id string) (*did.Document, error) {
	c.log.WithField("did", id).Info("retrieving DID document")

	// Parse the DID
	subject, err := did.Parse(id)
	if err != nil {
		return nil, err
	}

	// Extract the public key and application ID from the subject
	pub, appID, err := parseSubjectString(subject.Subject())
	if err != nil {
		return nil, err
	}

	// Retrieve the data from the network
	data, err := resolveDID(appID, pub, c.algod)
	if err != nil {
		return nil, err
	}
	doc := &did.Document{}
	if err := json.Unmarshal(data, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func (c *AlgoClient) submitToProvider(pub []byte, appID uint64, method string, doc []byte) error {
	c.log.Warning("using provider: ", c.np.StoreProvider)
	addr, err := addressFromPub(pub)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v1/%s/%d", c.np.StoreProvider, addr, appID)
	var payload io.Reader
	if doc != nil {
		payload = bytes.NewReader(doc)
	}
	req, err := http.NewRequestWithContext(context.TODO(), method, endpoint, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpC.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(res.Body) // nolint
	defer res.Body.Close()          // nolint
	if res.StatusCode != http.StatusOK {
		c.log.Warningf("%s", body)
		return fmt.Errorf("unexpected response: %s", res.Status)
	}
	return nil
}

func addressFromPub(pub []byte) (string, error) {
	return types.EncodeAddress(pub)
}

func parseSubjectString(subject string) (pub []byte, appID uint64, err error) {
	idSegments := strings.Split(subject, "-")
	if len(idSegments) != 2 {
		err = fmt.Errorf("invalid subject identifier")
		return
	}
	pub, err = hex.DecodeString(idSegments[0])
	if err != nil {
		err = fmt.Errorf("invalid public key in subject identifier")
		return
	}
	app, err := strconv.Atoi(idSegments[1])
	if err != nil {
		err = fmt.Errorf("invalid storage app ID in subject identifier")
		return
	}
	appID = uint64(app)
	return
}
