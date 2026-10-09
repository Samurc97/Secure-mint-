# Secure Mint — a Proof-of-Reserve-gated stablecoin minter on Chainlink CRE

**Bounty:** "Best workflow with CRE" (Chainlink, DoraHacks — BLI Legal Tech Hackathon 2)

A Chainlink Runtime Environment (CRE) workflow implementing the **Secure Mint**
pattern: a custom Proof-of-Reserve (PoR) oracle that **gates stablecoin minting
on-chain**. Every 5 minutes a Chainlink DON fetches the issuer's attested
reserve total, reads the stablecoin's on-chain `totalSupply()`, and submits a
DON-signed PoR report to the `SecureMintVault` receiver contract — whose
`mint()` **reverts unless the latest report proves full backing**
(reserves ≥ outstanding supply).

## Architecture

```
                        ┌─────────────────────────────────────────┐
                        │           Chainlink DON (CRE)             │
                        │                                         │
  ┌──────────────┐      │  ┌─────────┐   ┌──────────────────┐     │
  │ Reserve      │ HTTP │  │ Cron    │   │ EVM capability   │     │
  │ attestation  ├──────►   │ trigger │   │ eth_call         │     │
  │ API          │      │  │ (5 min) │   │ totalSupply()    │     │
  └──────────────┘      │  └────┬────┘   └────────┬─────────┘     │
                        │       │                 │               │
                        │       ▼                 ▼               │
                        │  ┌─────────────────────────────┐        │
                        │  │  Compare (DON consensus,    │        │
                        │  │  median aggregation)        │        │
                        │  │  healthy = reserves ≥ supply │        │
                        │  └──────────────┬──────────────┘        │
                        │                 │ DON-signed report     │
                        │                 │ (ecdsa / keccak256)   │
                        └─────────────────┼───────────────────────┘
                                          │ KeystoneForwarder
                                          ▼
                        ┌─────────────────────────────────────────┐
                        │ SecureMintVault (Sepolia)                 │
                        │  onReport(bytes,bytes) → stores          │
                        │    (totalReserve, totalSupply,           │
                        │     healthy, lastUpdated)                 │
                        │  mint(to, amount) → REVERTS unless       │
                        │    healthy && reserves ≥ supply + amount │
                        └─────────────────────────────────────────┘
```

**Report encoding** (Go `encodePoRBundle` ⇔ Solidity `abi.decode`):
`abi.encode(uint256 totalReserve, uint256 totalSupply, bool healthy, uint32 lastUpdated)` —
amounts in the stablecoin's base units (USDC: 6 decimals).

## Why regulators care (legal / compliance framing)

Stablecoin failures are reserve failures. Frameworks like the EU's **MiCA
(Title IV, Art. 35)** require issuers of asset-referenced tokens to maintain
adequate reserves and grant supervisors audit rights — but "adequate" is
usually proven by periodic, off-chain attestations that the public cannot
verify in real time.

Secure Mint turns the reserve requirement into an **on-chain, machine-readable
guardrail**:

- **Preventive, not detective.** Unbacked minting is *blocked at issuance
  time* (`ReserveShortfall` revert) instead of discovered months later in an
  audit.
- **Tamper-evident.** Every report is signed by the DON (threshold ECDSA);
  the full history of `(reserves, supply, healthy)` is emitted as
  `ReserveReport` events — a permanent audit trail any regulator can replay.
- **Auditor-friendly.** `issuanceAllowed(amount)` and the public
  `lastReserve` / `lastSupply` / `reservesHealthy` getters let supervisors
  check backing status without trusting the issuer's word.

## Project layout

```
cre-bounty/
├── por/
│   ├── workflow.go        # the Secure Mint workflow (cron → HTTP → EVM → report)
│   ├── workflow_test.go   # integration tests with REAL HTTP/EVM calls
│   ├── main.go            # WASM entrypoint (wasip1)
│   ├── workflow.yaml      # workflow settings (staging/production targets)
│   └── config.json        # schedule, URLs, addresses, gas limit
├── contracts/evm/src/
│   └── SecureMintVault.sol# IReceiver consumer: stores PoR, gates mint()
├── contracts/evm/abi/
│   ├── SecureMintVault.abi
│   └── SecureMintVault.bin # deployment bytecode (solc 0.8.30)
├── project.yaml           # chain RPCs (Sepolia via publicnode)
├── simulation.log         # validation log (see "Simulation status")
└── .env                   # placeholder key for simulation (gitignored)
```

## Setup

Requirements: Go ≥ 1.25.3, `cre` CLI v1.38.0
(`curl -sSfL https://cre.chain.link/install.sh | bash`).

```bash
cd cre-bounty
go mod download        # fetch the CRE Go SDK + go-ethereum
go build ./...         # compile check
```

The workflow compiles to the deployable WASM artifact with:

```bash
cd por && GOOS=wasip1 GOARCH=wasm go build -o por-workflow.wasm .
```

The contract compiles with solc 0.8.30:

```bash
solc --bin --abi --optimize -o build contracts/evm/src/SecureMintVault.sol
```

### Configuration (`por/config.json`)

| Key | Default | Meaning |
|---|---|---|
| `schedule` | `0 */5 * * * *` | cron trigger (every 5 min) |
| `reservesUrl` | CoinGecko USDC | reserve attestation endpoint (see below) |
| `stablecoinAddress` | `0x1c7D…7238` | USDC on Sepolia |
| `chainName` | `ethereum-testnet-sepolia` | chain-selector name |
| `receiverAddress` | `0x00…01` (placeholder) | **deploy `SecureMintVault.sol` and put its address here** |
| `gasLimit` | `1000000` | gas for the report write |

## Simulation status

`cre workflow simulate ./por --target staging-settings` currently **requires
authentication** (`cre login` opens an interactive browser flow; API keys are
issued at https://app.chain.link after human signup). Per the task's
instructions this step was not bypassed — see `simulation.log` for the exact
CLI output.

Instead, the workflow's data path is validated by **integration tests that
make real network calls** (`go test ./por/ -v`):

| Test | What it proves |
|---|---|
| `TestParseReservesLive` | the reserve endpoint responds and parses (73.17B USDC on 2026-10-09) |
| `TestTotalSupplyLive` | `eth_call totalSupply()` works against Sepolia USDC via the public RPC |
| `TestVerdictLogic` | healthy/shortfall/edge cases of the comparison |
| `TestVerdictLiveEndToEnd` | full fetch → read → verdict path on live data |
| `TestEncodeDecodeRoundTrip` | the Go ABI bundle decodes slot-for-slot as `SecureMintVault.onReport` expects |

Live run on 2026-10-09: `reserves=73166228374944020 supply=10742400244060817714
healthy=false` — the testnet USDC supply exceeds the attested figure, so the
workflow submits an **unhealthy** report and `mint()` stays gated. That is the
guardrail working as designed: the verdict follows the data, whichever way it
goes.

Once `cre login` is completed (human step), run the full DON simulation:

```bash
cre workflow simulate ./por --target staging-settings
```

## Plugging in a real reserve-attestation API

`reservesUrl` + `parseReserves()` are the only two things to change. Any HTTPS
endpoint returning JSON works; adapt the parser to its schema. Good candidates:

- **Issuer-published attestations** (e.g. Circle's monthly reserve reports /
  Grant Thornton JSON) — the compliance-grade source.
- **Custodian PoR APIs** such as the Verinumus real-time-reserves feed
  (`https://api.real-time-reserves.verinumus.io/v1/chainlink/proof-of-reserves/{token}`),
  which returns `{accountName, totalTrust, totalToken, updatedAt}` — a drop-in
  schema already close to `ReserveInfo`.
- **A self-hosted mock** for demos: any static JSON file with a reserve total.

The DON's median consensus aggregation (`consensus_aggregation:"median"`) stays
unchanged — it protects against a single compromised/malformed source.

## Deployment notes (not executed)

- Deploy `SecureMintVault.sol` to Sepolia with the KeystoneForwarder address,
  then set `receiverAddress` in `por/config.json`.
- `cre workflow deploy` requires Early Access approval (human browser signup
  at chain.link) — intentionally not attempted here.
- Fund the workflow owner key with Sepolia ETH only when broadcasting real
  report transactions.

## License

MIT (see LICENSE). Template scaffold adapted from
[smartcontractkit/cre-templates](https://github.com/smartcontractkit/cre-templates)
(`bring-your-own-data`, Go).
