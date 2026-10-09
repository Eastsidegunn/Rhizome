import Foundation

public final class SignerEngine {
    private let backend: SigningBackend
    private let store: SignerStore
    private let client: RhizomeClientProtocol
    private let logger: RequestLogger
    private let now: () -> Date
    private let nonce: () throws -> String

    public init(backend: SigningBackend, store: SignerStore, client: RhizomeClientProtocol, now: @escaping () -> Date = Date.init, nonce: @escaping () throws -> String = Randomness.nonce) {
        self.backend = backend
        self.store = store
        self.client = client
        self.now = now
        self.nonce = nonce
        self.logger = RequestLogger(url: store.logURL, now: now)
    }

    public func initializeKey() throws -> (anchor: Data, keyId: String) {
        try store.ensureDirectory()
        guard !FileManager.default.fileExists(atPath: store.keyURL.path), !FileManager.default.fileExists(atPath: store.anchorURL.path) else {
            throw SignerFailure(.refused, "signing key already exists")
        }
        let record = RequestLogRecord(consumer: "trust", decision: "key-init")
        let material = try logger.performPrompt(record) {
            try backend.createKey(verificationPrompt: "Rhizome 서명 키 확인")
        }
        let anchor = Canonical.anchorJSON(publicKeyDER: material.publicKeyDER)
        try store.writeNewKey(material.representation)
        try store.writeAnchor(anchor)
        return (anchor, Canonical.keyId(publicKeyDER: material.publicKeyDER))
    }

    public func showKey() throws -> (anchor: Data, keyId: String, publicKey: String) {
        let representation = try store.readKey()
        let publicDER = try backend.publicKeyDER(keyRepresentation: representation)
        return (Canonical.anchorJSON(publicKeyDER: publicDER), Canonical.keyId(publicKeyDER: publicDER), publicDER.base64EncodedString())
    }

    public func signDecision(gateId: String, requested: String, reason: String, correlationId: String = "") throws -> (spawn: Data, intent: Data) {
        let key = try loadedKey()
        let input: SigningInput
        do { input = try client.signingInput(gateId: gateId) }
        catch let failure as SignerFailure { throw failure.exit == .refused ? SignerFailure(.rhizomeReadFailed, failure.message) : failure }
        catch { throw SignerFailure(.rhizomeReadFailed, "cannot read signing input") }
        guard let journalId = input.journalId, !journalId.isEmpty else {
            throw SignerFailure(.rhizomeReadFailed, "anchorless serve: cannot sign")
        }
        do {
            try DecisionPolicy.requireSignableState(input)
            let mapping = try DecisionPolicy.map(consumer: input.consumer, requested: requested)
            try DecisionPolicy.requireReason(decision: mapping.decision, reason: reason)
            let prompt: String
            do {
                prompt = try PromptText.decision(consumer: input.consumer, gateId: input.gateId, title: input.title, decision: mapping.decision, reason: reason, requestDigest: input.requestDigest)
            } catch {
                try? logger.record(decisionRecord(input: input, decision: mapping.decision, reason: reason, correlationId: correlationId, keyId: key.keyId), result: "prompt-too-long")
                throw error
            }
            let signedAt = TimeFormat.string(now())
            let nonce = try nonce()
            let message = Canonical.decisionMessage(journalId: journalId, consumer: input.consumer, gateId: input.gateId, requestDigest: input.requestDigest, decision: mapping.decision, reason: reason, keyId: key.keyId, signedAt: signedAt, nonce: nonce)
            let signature = try logger.performPrompt(decisionRecord(input: input, decision: mapping.decision, reason: reason, correlationId: correlationId, keyId: key.keyId)) {
                try backend.sign(keyRepresentation: key.representation, message: message, prompt: prompt)
            }
            let envelope = SignatureEnvelope(keyId: key.keyId, signedAt: signedAt, nonce: nonce, sig: signature.base64EncodedString())
            let spawn = try JSONCoding.encode(SpawnOutput(digest: input.requestDigest, decision: mapping.decision, reason: reason, keyId: key.keyId, signature: envelope))
            let intent = try JSONCoding.encode(DecisionIntent(kind: mapping.kind, gateId: input.gateId, digest: input.requestDigest, reason: reason, actor: "signer", verification: VerificationIntent(signature: envelope)))
            return (spawn, intent)
        } catch let failure as SignerFailure where failure.exit == .notSignable {
            let decision = (try? DecisionPolicy.map(consumer: input.consumer, requested: requested).decision) ?? requested
            try? logger.record(decisionRecord(input: input, decision: decision, reason: reason, correlationId: correlationId, keyId: key.keyId), result: "not-signable")
            throw failure
        }
    }

    public func attestList(gateIds: [String]? = nil) throws -> (journalId: String?, items: [AttestItem], total: Int, digest: String) {
        let list = try client.unverifiedInputs()
        let selected = try AttestationPolicy.select(list.items, gateIds: gateIds)
        return (list.journalId, selected.items, selected.total, Canonical.manifestDigest(selected.items))
    }

    public func writeAttestReview(items: [AttestItem], total: Int, digest: String, to url: URL) throws {
        let review = AttestReview(manifestDigest: digest, count: items.count, totalAvailable: total, items: items.map {
            AttestReviewItem(consumer: $0.consumer, gateId: $0.gateId, decisionSequence: $0.decisionSequence, requestDigest: $0.digest, decision: $0.decision, reason: $0.reason, title: $0.title)
        })
        try store.writeReview(JSONCoding.encode(review, pretty: true), to: url)
    }

    public func signAttestation(gateIds: [String]? = nil, expected: String?) throws -> Data {
        let key = try loadedKey()
        let list = try attestList(gateIds: gateIds)
        guard let journalId = list.journalId, !journalId.isEmpty else {
            throw SignerFailure(.rhizomeReadFailed, "anchorless serve: cannot sign")
        }
        guard !list.items.isEmpty else { throw SignerFailure(.notSignable, "no gates to attest") }
        try AttestationPolicy.requireExpected(expected, actual: list.digest)
        let signedAt = TimeFormat.string(now())
        let nonce = try nonce()
        let message = Canonical.attestMessage(journalId: journalId, manifestDigest: list.digest, count: list.items.count, keyId: key.keyId, signedAt: signedAt, nonce: nonce)
        let record = RequestLogRecord(keyId: key.keyId, manifestDigest: list.digest, count: list.items.count, items: list.items.map {
            AttestLogItem(gateId: $0.gateId, decisionSequence: $0.decisionSequence, requestDigest: $0.digest, decision: $0.decision)
        })
        let signature = try logger.performPrompt(record) {
            try backend.sign(keyRepresentation: key.representation, message: message, prompt: PromptText.attest(count: list.items.count, manifestDigest: list.digest))
        }
        let envelope = SignatureEnvelope(keyId: key.keyId, signedAt: signedAt, nonce: nonce, sig: signature.base64EncodedString())
        let intentItems = list.items.map { AttestIntentItem(consumer: $0.consumer, gateId: $0.gateId, decisionSequence: $0.decisionSequence, digest: $0.digest, decision: $0.decision, reason: $0.reason) }
        return try JSONCoding.encode(AttestIntent(kind: "attest.create", manifestDigest: list.digest, items: intentItems, signature: envelope, actor: "signer"))
    }

    public func addKey(publicKeyDER: Data, principal: String) throws -> Data {
        guard principal == "H" else { throw SignerFailure(.refused, "principal must be H") }
        let key = try loadedKey()
        let list = try client.unverifiedInputs()
        guard let journalId = list.journalId, !journalId.isEmpty else { throw SignerFailure(.rhizomeReadFailed, "anchorless serve: cannot sign") }
        let incoming = try PublicKeyFile.inspect(der: publicKeyDER)
        let signedAt = TimeFormat.string(now())
        let nonce = try nonce()
        let message = Canonical.addMessage(journalId: journalId, keyId: incoming.keyId, algorithm: incoming.algorithm, publicKeyDER: publicKeyDER, principal: principal, assurance: "key", signingKeyId: key.keyId, signedAt: signedAt, nonce: nonce)
        let prompt = "키 추가 H\n\(incoming.keyId)\n\(incoming.algorithm)"
        guard prompt.count <= PromptText.maximumCharacters else { throw SignerFailure(.refused, "signing prompt exceeds 600 characters") }
        let record = RequestLogRecord(consumer: "trust", gateId: incoming.keyId, decision: "add", keyId: key.keyId)
        let signature = try logger.performPrompt(record) { try backend.sign(keyRepresentation: key.representation, message: message, prompt: prompt) }
        let envelope = SignatureEnvelope(keyId: key.keyId, signedAt: signedAt, nonce: nonce, sig: signature.base64EncodedString())
        let trust = TrustAdd(keyId: incoming.keyId, algorithm: incoming.algorithm, publicKey: publicKeyDER.base64EncodedString(), principal: principal, assurance: "key", signature: envelope)
        return try JSONCoding.encode(TrustAddIntent(kind: "trust.key.add", trust: trust, actor: "signer"))
    }

    public func revokeKey(keyId: String, reason: String) throws -> Data {
        guard !reason.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { throw SignerFailure(.refused, "revoke reason is required") }
        let key = try loadedKey()
        let list = try client.unverifiedInputs()
        guard let journalId = list.journalId, !journalId.isEmpty else { throw SignerFailure(.rhizomeReadFailed, "anchorless serve: cannot sign") }
        let signedAt = TimeFormat.string(now())
        let nonce = try nonce()
        let message = Canonical.revokeMessage(journalId: journalId, keyId: keyId, reason: reason, signingKeyId: key.keyId, signedAt: signedAt, nonce: nonce)
        let prompt = "키 폐기\n\(keyId)\n\(reason)"
        guard prompt.count <= PromptText.maximumCharacters else { throw SignerFailure(.refused, "signing prompt exceeds 600 characters") }
        let record = RequestLogRecord(consumer: "trust", gateId: keyId, decision: "revoke", reasonHash16: RequestLogger.reasonHash16(reason), keyId: key.keyId)
        let signature = try logger.performPrompt(record) { try backend.sign(keyRepresentation: key.representation, message: message, prompt: prompt) }
        let envelope = SignatureEnvelope(keyId: key.keyId, signedAt: signedAt, nonce: nonce, sig: signature.base64EncodedString())
        return try JSONCoding.encode(TrustRevokeIntent(kind: "trust.key.revoke", trust: TrustRevoke(keyId: keyId, reason: reason, signature: envelope), actor: "signer"))
    }

    public func submit(_ intent: Data) throws -> Data {
        let response = try client.submit(intent: intent)
        if let object = try? JSONSerialization.jsonObject(with: response) as? [String: Any],
           let accepted = (object["Accepted"] ?? object["accepted"]) as? Bool,
           !accepted {
            throw SignerFailure(.rhizomeReadFailed, "Rhizome refused intent")
        }
        return response
    }

    private func loadedKey() throws -> (representation: Data, publicDER: Data, keyId: String) {
        let representation = try store.readKey()
        let publicDER = try backend.publicKeyDER(keyRepresentation: representation)
        return (representation, publicDER, Canonical.keyId(publicKeyDER: publicDER))
    }

    private func decisionRecord(input: SigningInput, decision: String, reason: String, correlationId: String, keyId: String) -> RequestLogRecord {
        RequestLogRecord(consumer: input.consumer, gateId: input.gateId, decision: decision, reasonHash16: RequestLogger.reasonHash16(reason), requestDigest: input.requestDigest, correlationId: correlationId, keyId: keyId)
    }
}

public enum JSONCoding {
    public static func encode<T: Encodable>(_ value: T, pretty: Bool = false) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = pretty ? [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes] : [.sortedKeys, .withoutEscapingSlashes]
        return try encoder.encode(value)
    }
}

private struct SpawnOutput: Codable { let digest, decision, reason, keyId: String; let signature: SignatureEnvelope }
private struct VerificationIntent: Codable { let signature: SignatureEnvelope }
private struct DecisionIntent: Codable { let kind, gateId, digest, reason, actor: String; let verification: VerificationIntent }
private struct AttestIntentItem: Codable { let consumer, gateId: String; let decisionSequence: UInt64; let digest, decision, reason: String }
private struct AttestIntent: Codable { let kind, manifestDigest: String; let items: [AttestIntentItem]; let signature: SignatureEnvelope; let actor: String }
private struct TrustAdd: Codable { let keyId, algorithm, publicKey, principal, assurance: String; let signature: SignatureEnvelope }
private struct TrustAddIntent: Codable { let kind: String; let trust: TrustAdd; let actor: String }
private struct TrustRevoke: Codable { let keyId, reason: String; let signature: SignatureEnvelope }
private struct TrustRevokeIntent: Codable { let kind: String; let trust: TrustRevoke; let actor: String }
private struct AttestReviewItem: Codable { let consumer, gateId: String; let decisionSequence: UInt64; let requestDigest, decision, reason, title: String }
private struct AttestReview: Codable { let manifestDigest: String; let count, totalAvailable: Int; let items: [AttestReviewItem] }
