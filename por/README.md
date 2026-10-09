# Secure Mint workflow (Go)

Cron-triggered Proof-of-Reserve workflow: fetches the issuer's attested
reserve total (HTTP), reads the stablecoin's `totalSupply()` on Sepolia (EVM),
and submits a DON-signed PoR report to `SecureMintVault`, whose `mint()`
reverts unless reserves fully back the supply.

See the [project README](../README.md) for architecture, setup, legal
framing, and validation. Run the integration tests with:

```bash
go test ./ -v
```
