// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title SecureMintVault
/// @notice RegTech guardrail for stablecoin issuance, fed by a Chainlink CRE
/// "Secure Mint" workflow.
///
/// Every few minutes a Chainlink DON:
///   1. fetches the issuer's attested reserve total off-chain (HTTP),
///   2. reads the stablecoin's on-chain totalSupply() (EVM),
///   3. compares reserves vs supply, and
///   4. submits a DON-signed Proof-of-Reserve report to this contract via the
///      KeystoneForwarder, which calls onReport().
///
/// mint() REVERTS unless the latest DON-signed report proves full backing
/// (reserves >= outstanding supply). This enforces a MiCA-style reserve
/// requirement (cf. MiCA Title IV, Art. 35: issuers of asset-referenced
/// tokens must maintain adequate reserves) directly on-chain, giving
/// regulators and auditors a tamper-evident, machine-readable proof that
/// unbacked tokens cannot be minted.
///
/// Report encoding (must match the Go workflow's encodePoRBundle):
///   abi.encode(uint256 totalReserve, uint256 totalSupply, bool healthy, uint32 lastUpdated)
/// All amounts are in the stablecoin's base units (USDC: 6 decimals).
interface IReceiver {
    function onReport(bytes calldata metadata, bytes calldata report) external;
}

contract SecureMintVault is IReceiver {
    /// @notice The KeystoneForwarder allowed to deliver DON-signed reports.
    address public immutable forwarder;

    /// @notice Latest DON-attested reserve total (base units).
    uint256 public lastReserve;
    /// @notice On-chain totalSupply observed at report time (base units).
    uint256 public lastSupply;
    /// @notice True when the DON proved reserves >= supply.
    bool public reservesHealthy;
    /// @notice Timestamp of the latest report (unix seconds).
    uint32 public lastUpdated;
    /// @notice Total amount minted through this vault (demo accounting).
    uint256 public totalMinted;

    error OnlyForwarder();
    error ReserveShortfall(uint256 reserves, uint256 supply);
    error StaleReport(uint32 lastUpdated);

    event ReserveReport(uint256 reserves, uint256 supply, bool healthy, uint32 updatedAt);
    event Minted(address indexed to, uint256 amount);

    /// @param _forwarder Address of the KeystoneForwarder for the target chain.
    constructor(address _forwarder) {
        forwarder = _forwarder;
    }

    /// @notice Receives DON-signed PoR reports from the KeystoneForwarder.
    /// @dev The forwarder authenticates the DON signatures before this call,
    /// so we only need to check msg.sender.
    function onReport(bytes calldata /*metadata*/, bytes calldata report) external {
        if (msg.sender != forwarder) revert OnlyForwarder();

        (uint256 totalReserve, uint256 totalSupply, bool healthy, uint32 updatedAt) =
            abi.decode(report, (uint256, uint256, bool, uint32));

        lastReserve = totalReserve;
        lastSupply = totalSupply;
        reservesHealthy = healthy;
        lastUpdated = updatedAt;

        emit ReserveReport(totalReserve, totalSupply, healthy, updatedAt);
    }

    /// @notice Mint (demo) stablecoins, gated by the latest PoR report.
    /// @dev In production this would call the stablecoin's mint(); here the
    /// vault itself is the issuance gate so the guardrail is auditable.
    /// @param to Recipient of the minted amount.
    /// @param amount Amount to mint, in base units.
    function mint(address to, uint256 amount) external {
        if (!reservesHealthy || lastReserve < lastSupply + amount) {
            revert ReserveShortfall(lastReserve, lastSupply);
        }
        totalMinted += amount;
        emit Minted(to, amount);
    }

    /// @notice Convenience view for auditors/regulators: is issuance currently allowed?
    function issuanceAllowed(uint256 amount) external view returns (bool) {
        return reservesHealthy && lastReserve >= lastSupply + amount;
    }
}
