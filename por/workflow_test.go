package main

// Integration tests for the Secure Mint workflow's data path.
//
// These tests make REAL network calls:
//   - HTTP GET to the configured reserve attestation endpoint (CoinGecko),
//   - eth_call totalSupply() against Sepolia via the public RPC from project.yaml.
//
// They validate the exact parsing / scaling / comparison / ABI-encoding code
// paths the workflow uses. The DON plumbing (consensus aggregation, report
// signing, KeystoneForwarder write) is exercised by `cre workflow simulate`
// (requires `cre login`; see README).

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	testReservesURL         = "https://api.coingecko.com/api/v3/coins/usd-coin"
	testStablecoin          = "0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238" // USDC on Sepolia
	testSepoliaRPC          = "https://ethereum-sepolia-rpc.publicnode.com"
	testReceiverPlaceholder = "0x0000000000000000000000000000000000000001"
)

func TestParseReservesLive(t *testing.T) {
	resp, err := http.Get(testReservesURL)
	if err != nil {
		t.Fatalf("HTTP GET reserves URL failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	info, err := parseReserves(body, nil)
	if err != nil {
		t.Fatalf("parseReserves failed: %v", err)
	}
	if info.TotalReserve.IsZero() || info.TotalReserve.IsNegative() {
		t.Fatalf("unexpected reserve total: %s", info.TotalReserve)
	}
	if info.LastUpdated.IsZero() || info.LastUpdated.After(time.Now().Add(time.Hour)) {
		t.Fatalf("unexpected lastUpdated: %s", info.LastUpdated)
	}
	t.Logf("attested reserves: %s USDC (as of %s)", info.TotalReserve, info.LastUpdated.Format(time.RFC3339))

	scaled := scaleReserves(info.TotalReserve)
	if scaled.Sign() <= 0 {
		t.Fatalf("scaled reserves not positive: %s", scaled)
	}
	t.Logf("scaled reserves (base units): %s", scaled)
}

func TestTotalSupplyLive(t *testing.T) {
	client, err := ethclient.Dial(testSepoliaRPC)
	if err != nil {
		t.Fatalf("dialing Sepolia RPC: %v", err)
	}
	defer client.Close()

	addr := common.HexToAddress(testStablecoin)
	data, err := client.CallContract(context.Background(), ethereum.CallMsg{
		To:   &addr,
		Data: totalSupplySelector,
	}, nil)
	if err != nil {
		t.Fatalf("eth_call totalSupply failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("empty return data; is %s a contract on Sepolia?", testStablecoin)
	}
	supply := new(big.Int).SetBytes(data)
	if supply.Sign() < 0 {
		t.Fatalf("negative supply: %s", supply)
	}
	t.Logf("USDC totalSupply on Sepolia: %s base units", supply)
}

func TestVerdictLogic(t *testing.T) {
	// Healthy: reserves above supply.
	if !checkHealthy(big.NewInt(200_000_000), big.NewInt(150_000_000)) {
		t.Fatal("expected healthy=true when reserves > supply")
	}
	// Edge: exactly equal is healthy (fully backed).
	if !checkHealthy(big.NewInt(100), big.NewInt(100)) {
		t.Fatal("expected healthy=true when reserves == supply")
	}
	// Shortfall: mint must stay gated.
	if checkHealthy(big.NewInt(99_999_999), big.NewInt(100_000_000)) {
		t.Fatal("expected healthy=false on reserve shortfall")
	}
}

func TestVerdictLiveEndToEnd(t *testing.T) {
	// Full data path with live inputs: fetch reserves, read supply, verdict.
	resp, err := http.Get(testReservesURL)
	if err != nil {
		t.Fatalf("HTTP GET reserves URL failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	info, err := parseReserves(body, nil)
	if err != nil {
		t.Fatalf("parseReserves failed: %v", err)
	}

	client, err := ethclient.Dial(testSepoliaRPC)
	if err != nil {
		t.Fatalf("dialing Sepolia RPC: %v", err)
	}
	defer client.Close()
	addr := common.HexToAddress(testStablecoin)
	data, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &addr, Data: totalSupplySelector}, nil)
	if err != nil {
		t.Fatalf("eth_call totalSupply failed: %v", err)
	}
	supply := new(big.Int).SetBytes(data)

	reserveScaled := scaleReserves(info.TotalReserve)
	healthy := checkHealthy(reserveScaled, supply)
	t.Logf("verdict: reserves=%s supply=%s healthy=%v", reserveScaled, supply, healthy)
	// The verdict must follow the data, whichever way it goes: the guardrail
	// gates minting exactly when reserves < supply.
	if want := reserveScaled.Cmp(supply) >= 0; healthy != want {
		t.Fatalf("verdict inconsistent: healthy=%v, want %v", healthy, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// The Go-encoded bundle must decode exactly as SecureMintVault.onReport
	// decodes it: (uint256 totalReserve, uint256 totalSupply, bool healthy, uint32 lastUpdated).
	// We verify slot-by-slot, the same way the EVM ABI decoder reads them.
	wantReserve := big.NewInt(70_000_000_000_000_000) // 70B USDC in base units
	wantSupply := big.NewInt(12_345_678_000_000)
	want := PoRBundle{
		TotalReserve: wantReserve,
		TotalSupply:  wantSupply,
		Healthy:      true,
		LastUpdated:  1791508800,
	}
	encoded, err := encodePoRBundle(want)
	if err != nil {
		t.Fatalf("encodePoRBundle failed: %v", err)
	}
	if len(encoded) != 4*32 {
		t.Fatalf("expected 4 ABI slots (128 bytes), got %d", len(encoded))
	}

	gotReserve := new(big.Int).SetBytes(encoded[0:32])
	gotSupply := new(big.Int).SetBytes(encoded[32:64])
	gotHealthy := encoded[63+32] == 1 // bool occupies the last byte of its slot
	gotUpdated := new(big.Int).SetBytes(encoded[96:128]).Uint64()

	if gotReserve.Cmp(wantReserve) != 0 {
		t.Fatalf("slot0 totalReserve mismatch: got %s", gotReserve)
	}
	if gotSupply.Cmp(wantSupply) != 0 {
		t.Fatalf("slot1 totalSupply mismatch: got %s", gotSupply)
	}
	if gotHealthy != want.Healthy {
		t.Fatalf("slot2 healthy mismatch: got %v", gotHealthy)
	}
	if uint32(gotUpdated) != want.LastUpdated {
		t.Fatalf("slot3 lastUpdated mismatch: got %d", gotUpdated)
	}
	t.Logf("bundle ABI layout OK (%d bytes): matches SecureMintVault.onReport decoding", len(encoded))
}
