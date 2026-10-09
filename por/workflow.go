package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/blockchain/evm"
	"github.com/smartcontractkit/cre-sdk-go/capabilities/networking/http"
	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"
)

// totalSupply() function selector (first 4 bytes of keccak256("totalSupply()"))
var totalSupplySelector = crypto.Keccak256([]byte("totalSupply()"))[:4]

// usdcDecimals is the number of decimals of the tracked stablecoin (USDC = 6).
// Attested reserve totals (whole tokens) are scaled by 10^usdcDecimals so they
// can be compared 1:1 with the on-chain totalSupply (base units).
const usdcDecimals = 6

// Config holds the workflow configuration (see config.json).
type Config struct {
	Schedule          string `json:"schedule"`
	ReservesURL       string `json:"reservesUrl"`
	StablecoinAddress string `json:"stablecoinAddress"`
	ChainName         string `json:"chainName"`
	ReceiverAddress   string `json:"receiverAddress"`
	GasLimit          uint64 `json:"gasLimit"`
}

// ReserveInfo is the DON-aggregated reserve attestation.
type ReserveInfo struct {
	LastUpdated  time.Time       `consensus_aggregation:"median" json:"lastUpdated"`
	TotalReserve decimal.Decimal `consensus_aggregation:"median" json:"totalReserve"`
}

// coinGeckoResp is the subset of the CoinGecko /coins/{id} response we need.
// In production this is swapped for the issuer's attestation API (see README).
type coinGeckoResp struct {
	MarketData struct {
		TotalSupply float64 `json:"total_supply"`
		LastUpdated string  `json:"last_updated"`
	} `json:"market_data"`
}

// PoRBundle is the report payload decoded by SecureMintVault.onReport.
// Field order MUST match the Solidity tuple:
// (uint256 totalReserve, uint256 totalSupply, bool healthy, uint32 lastUpdated)
type PoRBundle struct {
	TotalReserve *big.Int
	TotalSupply  *big.Int
	Healthy      bool
	LastUpdated  uint32
}

func InitWorkflow(config *Config, logger *slog.Logger, secretsProvider cre.SecretsProvider) (cre.Workflow[*Config], error) {
	workflow := cre.Workflow[*Config]{
		cre.Handler(
			cron.Trigger(&cron.Config{Schedule: config.Schedule}),
			onCronTrigger,
		),
	}
	return workflow, nil
}

func onCronTrigger(config *Config, runtime cre.Runtime, outputs *cron.Payload) (string, error) {
	summary, err := doSecureMint(config, runtime, outputs.ScheduledExecutionTime.AsTime())
	if err != nil {
		runtime.Logger().Error("secure-mint workflow failed", "err", err)
		return "", err
	}
	return summary, nil
}

// doSecureMint is the "Secure Mint" pattern:
//  1. fetch the issuer's attested reserve total via HTTP (DON consensus),
//  2. read the stablecoin's on-chain totalSupply via the EVM capability,
//  3. compare reserves vs supply,
//  4. submit a DON-signed PoR report to the SecureMintVault receiver contract,
//     whose mint() reverts unless the latest report proves full backing.
func doSecureMint(config *Config, runtime cre.Runtime, runTime time.Time) (string, error) {
	logger := runtime.Logger()
	logger.Info("secure-mint tick",
		"reservesURL", config.ReservesURL,
		"stablecoin", config.StablecoinAddress,
		"chain", config.ChainName,
		"time", runTime)

	// 1. Attested reserves (HTTP capability, median across DON nodes).
	reserveInfo, err := http.SendRequest(
		config,
		runtime,
		&http.Client{},
		fetchReserves,
		cre.ConsensusAggregationFromTags[*ReserveInfo]()).Await()
	if err != nil {
		return "", fmt.Errorf("fetching reserves: %w", err)
	}
	logger.Info("attested reserves", "totalReserve", reserveInfo.TotalReserve, "asOf", reserveInfo.LastUpdated)

	// 2. On-chain outstanding supply.
	supply, err := readTotalSupply(config, runtime)
	if err != nil {
		return "", fmt.Errorf("reading totalSupply: %w", err)
	}
	logger.Info("on-chain supply", "totalSupply", supply)

	// 3. Compare in base units (USDC: 6 decimals).
	reserveScaled := scaleReserves(reserveInfo.TotalReserve)
	healthy := checkHealthy(reserveScaled, supply)
	logger.Info("PoR verdict",
		"reservesBaseUnits", reserveScaled,
		"supplyBaseUnits", supply,
		"healthy", healthy)
	if !healthy {
		logger.Error("RESERVE SHORTFALL: attested reserves are below outstanding supply; submitting unhealthy report so mint() stays gated")
	}

	// 4. DON-signed on-chain report to the SecureMintVault receiver.
	if err := submitPoRReport(config, runtime, reserveScaled, supply, healthy, reserveInfo.LastUpdated); err != nil {
		return "", fmt.Errorf("submitting PoR report: %w", err)
	}

	return fmt.Sprintf("reserves=%s supply=%s healthy=%v", reserveScaled.String(), supply.String(), healthy), nil
}

// fetchReserves runs on each DON node: GET the reserve attestation endpoint
// and parse the reserve total. Swap the URL + parser for a real attestation
// API (e.g. the issuer's custodian report) — see README.
func fetchReserves(config *Config, logger *slog.Logger, sendRequester *http.SendRequester) (*ReserveInfo, error) {
	httpOut, err := sendRequester.SendRequest(&http.Request{
		Method: "GET",
		Url:    config.ReservesURL,
	}).Await()
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	return parseReserves(httpOut.Body, logger)
}

// parseReserves parses a reserve attestation payload into a ReserveInfo.
// Pure function (no runtime dependency) so it can be unit-tested.
func parseReserves(body []byte, logger *slog.Logger) (*ReserveInfo, error) {
	var cg coinGeckoResp
	if err := json.Unmarshal(body, &cg); err != nil {
		return nil, fmt.Errorf("parsing reserve response: %w", err)
	}
	if cg.MarketData.TotalSupply <= 0 {
		return nil, fmt.Errorf("invalid total_supply in reserve response")
	}

	updatedAt, err := time.Parse(time.RFC3339, cg.MarketData.LastUpdated)
	if err != nil {
		if logger != nil {
			logger.Warn("could not parse last_updated, using now", "value", cg.MarketData.LastUpdated)
		}
		updatedAt = time.Now().UTC()
	}

	return &ReserveInfo{
		LastUpdated:  updatedAt.UTC(),
		TotalReserve: decimal.NewFromFloat(cg.MarketData.TotalSupply),
	}, nil
}

// scaleReserves converts a whole-token reserve total into base units
// (USDC: 6 decimals) for 1:1 comparison with on-chain totalSupply.
func scaleReserves(totalReserve decimal.Decimal) *big.Int {
	return totalReserve.Mul(decimal.NewFromInt(1).Shift(int32(usdcDecimals))).BigInt()
}

// checkHealthy reports whether attested reserves fully back the outstanding supply.
func checkHealthy(reserveBaseUnits, supplyBaseUnits *big.Int) bool {
	return reserveBaseUnits.Cmp(supplyBaseUnits) >= 0
}

// readTotalSupply calls totalSupply() on the stablecoin contract via the EVM
// capability (eth_call against the configured chain RPC).
func readTotalSupply(config *Config, runtime cre.Runtime) (*big.Int, error) {
	chainSelector, err := evm.ChainSelectorFromName(config.ChainName)
	if err != nil {
		return nil, fmt.Errorf("resolving chain %q: %w", config.ChainName, err)
	}

	reply, err := (&evm.Client{ChainSelector: chainSelector}).CallContract(
		runtime,
		&evm.CallContractRequest{
			Call: &evm.CallMsg{
				From: make([]byte, 20),
				To:   common.HexToAddress(config.StablecoinAddress).Bytes(),
				Data: totalSupplySelector,
			},
		},
	).Await()
	if err != nil {
		return nil, fmt.Errorf("EVM CallContract failed: %w", err)
	}
	if len(reply.Data) == 0 {
		return nil, fmt.Errorf("empty return data from totalSupply(); is %s a contract on %s?",
			config.StablecoinAddress, config.ChainName)
	}

	return new(big.Int).SetBytes(reply.Data), nil
}

// submitPoRReport ABI-encodes the PoR bundle, asks the DON to sign it, and
// writes it to the SecureMintVault receiver via KeystoneForwarder.
func submitPoRReport(config *Config, runtime cre.Runtime, reserves, supply *big.Int, healthy bool, updatedAt time.Time) error {
	logger := runtime.Logger()

	chainSelector, err := evm.ChainSelectorFromName(config.ChainName)
	if err != nil {
		return fmt.Errorf("resolving chain %q: %w", config.ChainName, err)
	}
	evmClient := &evm.Client{ChainSelector: chainSelector}

	encoded, err := encodePoRBundle(PoRBundle{
		TotalReserve: reserves,
		TotalSupply:  supply,
		Healthy:      healthy,
		LastUpdated:  uint32(updatedAt.UTC().Unix()),
	})
	if err != nil {
		return fmt.Errorf("encoding PoR bundle: %w", err)
	}

	logger.Info("requesting DON-signed report")
	report, err := runtime.GenerateReport(&cre.ReportRequest{
		EncodedPayload: encoded,
		EncoderName:    "evm",
		SigningAlgo:    "ecdsa",
		HashingAlgo:    "keccak256",
	}).Await()
	if err != nil {
		return fmt.Errorf("generating report: %w", err)
	}
	logger.Info("report signed by DON")

	logger.Info("writing report on-chain", "receiver", config.ReceiverAddress)
	resp, err := evmClient.WriteReport(runtime, &evm.WriteCreReportRequest{
		Receiver:  common.HexToAddress(config.ReceiverAddress).Bytes(),
		Report:    report,
		GasConfig: &evm.GasConfig{GasLimit: config.GasLimit},
	}).Await()
	if err != nil {
		return fmt.Errorf("WriteReport failed: %w", err)
	}
	if resp.TxStatus != evm.TxStatus_TX_STATUS_SUCCESS {
		msg := "unknown error"
		if resp.ErrorMessage != nil {
			msg = *resp.ErrorMessage
		}
		return fmt.Errorf("report transaction failed (status %v): %s", resp.TxStatus, msg)
	}
	if status := resp.ReceiverContractExecutionStatus; status != nil &&
		*status != evm.ReceiverContractExecutionStatus_RECEIVER_CONTRACT_EXECUTION_STATUS_SUCCESS {
		return fmt.Errorf("receiver contract execution failed")
	}
	logger.Info("PoR report written",
		"txHash", common.BytesToHash(resp.TxHash).Hex(),
		"chain", config.ChainName)

	return nil
}

// encodePoRBundle ABI-encodes the bundle exactly as SecureMintVault.onReport
// decodes it: (uint256 totalReserve, uint256 totalSupply, bool healthy, uint32 lastUpdated).
func encodePoRBundle(in PoRBundle) ([]byte, error) {
	tupleType, err := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "totalReserve", Type: "uint256"},
		{Name: "totalSupply", Type: "uint256"},
		{Name: "healthy", Type: "bool"},
		{Name: "lastUpdated", Type: "uint32"},
	})
	if err != nil {
		return nil, fmt.Errorf("building tuple type: %w", err)
	}
	args := abi.Arguments{{Name: "report", Type: tupleType}}
	return args.Pack(in)
}
