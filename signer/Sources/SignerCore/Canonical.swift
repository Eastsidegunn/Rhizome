import CryptoKit
import Foundation

public enum Canonical {
    public static let decisionTag = "rhz-gate-decision-v1"
    public static let attestTag = "rhz-gate-attest-v1"
    public static let trustKeyTag = "rhz-trust-key-v1"

    public static func lengthPrefixed(_ fields: [Data]) -> Data {
        var output = Data()
        for field in fields {
            precondition(field.count <= Int(UInt32.max))
            var length = UInt32(field.count).bigEndian
            withUnsafeBytes(of: &length) { output.append(contentsOf: $0) }
            output.append(field)
        }
        return output
    }

    public static func lengthPrefixed(_ fields: [String]) -> Data {
        lengthPrefixed(fields.map { Data($0.utf8) })
    }

    public static func decisionMessage(journalId: String, consumer: String, gateId: String, requestDigest: String, decision: String, reason: String, keyId: String, signedAt: String, nonce: String) -> Data {
        lengthPrefixed([decisionTag, journalId, consumer, gateId, requestDigest, decision, reason, keyId, signedAt, nonce])
    }

    public static func attestManifestBytes(_ items: [AttestItem]) -> Data {
        let encoded = items.map {
            lengthPrefixed([$0.consumer, $0.gateId, String($0.decisionSequence), $0.digest, $0.decision, $0.reason])
        }
        return lengthPrefixed(encoded)
    }

    public static func manifestDigest(_ items: [AttestItem]) -> String {
        "sha256:" + SHA256.hash(data: attestManifestBytes(items)).hex
    }

    public static func attestMessage(journalId: String, manifestDigest: String, count: Int, keyId: String, signedAt: String, nonce: String) -> Data {
        lengthPrefixed([attestTag, journalId, manifestDigest, String(count), keyId, signedAt, nonce])
    }

    public static func addMessage(journalId: String, keyId: String, algorithm: String, publicKeyDER: Data, principal: String, assurance: String, signingKeyId: String, signedAt: String, nonce: String) -> Data {
        lengthPrefixed([
            Data(trustKeyTag.utf8), Data(journalId.utf8), Data("add".utf8), Data(keyId.utf8), Data(algorithm.utf8), publicKeyDER,
            Data(principal.utf8), Data(assurance.utf8), Data(signingKeyId.utf8), Data(signedAt.utf8), Data(nonce.utf8),
        ])
    }

    public static func revokeMessage(journalId: String, keyId: String, reason: String, signingKeyId: String, signedAt: String, nonce: String) -> Data {
        lengthPrefixed([trustKeyTag, journalId, "revoke", keyId, reason, signingKeyId, signedAt, nonce])
    }

    public static func keyId(publicKeyDER: Data) -> String {
        "sha256:" + SHA256.hash(data: publicKeyDER).hex
    }

    public static func anchorJSON(publicKeyDER: Data) -> Data {
        let value = "{\"format\":\"rhizome-trust-anchor-v1\",\"principal\":\"H\",\"algorithm\":\"ecdsa-p256\",\"assurance\":\"key\",\"publicKey\":\"\(publicKeyDER.base64EncodedString())\"}"
        return Data(value.utf8)
    }
}

public extension Digest {
    var hex: String { map { String(format: "%02x", $0) }.joined() }
}

public extension Data {
    init?(hex: String) {
        guard hex.count.isMultiple(of: 2) else { return nil }
        var result = Data(capacity: hex.count / 2)
        var index = hex.startIndex
        while index < hex.endIndex {
            let next = hex.index(index, offsetBy: 2)
            guard let byte = UInt8(hex[index..<next], radix: 16) else { return nil }
            result.append(byte)
            index = next
        }
        self = result
    }

    var hex: String { map { String(format: "%02x", $0) }.joined() }
}
