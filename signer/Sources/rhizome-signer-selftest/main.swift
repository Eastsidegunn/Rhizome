import CryptoKit
import Darwin
import Foundation
import SignerCore

private let journalId = "00112233445566778899aabbccddeeff"
private let vectorKeyId = "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9"
private let questionGateId = "q-92caa77d4e8cb1f9a861ff35"
private let questionDigest = "rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61"
private let approvalGateId = "appr-111111111111111111111111"
private let approvalDigest = "hx-args-digest-v1:opaque-vector"

private struct AssertionFailure: Error, CustomStringConvertible {
    let message: String
    var description: String { message }
}

private func expect(_ condition: @autoclosure () throws -> Bool, _ message: String = "assertion failed", file: StaticString = #fileID, line: UInt = #line) throws {
    guard try condition() else { throw AssertionFailure(message: "\(file):\(line): \(message)") }
}

private func decisionMessageMatchesGoVector() throws {
    let message = Canonical.decisionMessage(
        journalId: journalId,
        consumer: "question",
        gateId: "q-92caa77d4e8cb1f9a861ff35",
        requestDigest: "rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61",
        decision: "approve",
        reason: "",
        keyId: vectorKeyId,
        signedAt: "2026-10-08T00:00:00Z",
        nonce: "000102030405060708090a0b0c0d0e0f"
    )
    try expect(message.count == 336)
    try expect(SHA256.hash(data: message).hex == "9bebf41811aa4a1f080514aebf2a9fc87afe3edd17e90cc7581feba3a3a07570")
}

private func attestN1AndN3GoGeneratedFixtures() throws {
    let n1 = vectorN1()
    let n3 = n1 + [
        AttestItem(consumer: "approval", gateId: "appr-111111111111111111111111", decisionSequence: 11, digest: "hx-args-digest-v1:vector-approval", decision: "allow", reason: "approved by operator"),
        AttestItem(consumer: "question", gateId: "q-222222222222222222222222", decisionSequence: 19, digest: "rhz-question-v2:2222222222222222222222222222222222222222222222222222222222222222", decision: "reject", reason: "needs revision"),
    ]
    let n1Manifest = "00000092000000087175657374696f6e0000001a712d39326361613737643465386362316639613836316666333500000001370000005072687a2d7175657374696f6e2d76323a3932636161373764346538636231663961383631666633356130386334326561376363343862323066646261636239313163626638353830616461666562363100000007617070726f766500000000"
    let n1Message = "0000001272687a2d676174652d6174746573742d7631000000203030313132323333343435353636373738383939616162626363646465656666000000477368613235363a666331333537656433643964373530356261333866366635613936653739373739386131663137363563346265373164316162343830383336376365656566380000000131000000477368613235363a3036653366643866646132396262363061623539353537646536316564623061656364623233313133346265333065373562343535663865316237393266613900000014323032362d31302d30395430303a30303a30305a000000203130313131323133313431353136313731383139316131623163316431653166"
    let n3Manifest = "00000092000000087175657374696f6e0000001a712d39326361613737643465386362316639613836316666333500000001370000005072687a2d7175657374696f6e2d76323a3932636161373764346538636231663961383631666633356130386334326561376363343862323066646261636239313163626638353830616461666562363100000007617070726f7665000000000000007900000008617070726f76616c0000001d617070722d3131313131313131313131313131313131313131313131310000000231310000002168782d617267732d6469676573742d76313a766563746f722d617070726f76616c00000005616c6c6f7700000014617070726f766564206279206f70657261746f72000000a0000000087175657374696f6e0000001a712d3232323232323232323232323232323232323232323232320000000231390000005072687a2d7175657374696f6e2d76323a323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232320000000672656a6563740000000e6e65656473207265766973696f6e"
    let n3Message = "0000001272687a2d676174652d6174746573742d7631000000203030313132323333343435353636373738383939616162626363646465656666000000477368613235363a666137653431333333643332346366386530383965343535653431616635626266353235373133316165386430393865376436356330633737643933343231370000000133000000477368613235363a3036653366643866646132396262363061623539353537646536316564623061656364623233313133346265333065373562343535663865316237393266613900000014323032362d31302d30395430303a30303a30305a000000203230323132323233323432353236323732383239326132623263326432653266"
    try expect(Canonical.attestManifestBytes(n1).hex == n1Manifest)
    try expect(Canonical.attestMessage(journalId: journalId, manifestDigest: Canonical.manifestDigest(n1), count: 1, keyId: vectorKeyId, signedAt: "2026-10-09T00:00:00Z", nonce: "101112131415161718191a1b1c1d1e1f").hex == n1Message)
    try expect(Canonical.attestManifestBytes(n3).hex == n3Manifest)
    try expect(Canonical.attestMessage(journalId: journalId, manifestDigest: Canonical.manifestDigest(n3), count: 3, keyId: vectorKeyId, signedAt: "2026-10-09T00:00:00Z", nonce: "202122232425262728292a2b2c2d2e2f").hex == n3Message)
    try expect(Canonical.manifestDigest(n1) == "sha256:fc1357ed3d9d7505ba38f6f5a96e797798a1f1765c4be71d1ab4808367ceeef8")
    try expect(Canonical.manifestDigest(n3) == "sha256:fa7e41333d324cf8e089e455e41af5bbf5257131ae8d098e7d65c0c77d934217")
}

private func addAndRevokeGoGeneratedFixtures() throws {
    let add = Canonical.addMessage(journalId: journalId, keyId: "sha256:new", algorithm: "ed25519", publicKeyDER: Data([1, 2, 3]), principal: "H", assurance: "key", signingKeyId: "sha256:signer", signedAt: "2026-10-08T00:00:00Z", nonce: "000102030405060708090a0b0c0d0e0f")
    let revoke = Canonical.revokeMessage(journalId: journalId, keyId: "sha256:target", reason: "lost", signingKeyId: "sha256:signer", signedAt: "2026-10-08T00:00:00Z", nonce: "000102030405060708090a0b0c0d0e0f")
    try expect(add.hex == "0000001072687a2d74727573742d6b65792d7631000000203030313132323333343435353636373738383939616162626363646465656666000000036164640000000a7368613235363a6e65770000000765643235353139000000030102030000000148000000036b65790000000d7368613235363a7369676e657200000014323032362d31302d30385430303a30303a30305a000000203030303130323033303430353036303730383039306130623063306430653066")
    try expect(revoke.hex == "0000001072687a2d74727573742d6b65792d7631000000203030313132323333343435353636373738383939616162626363646465656666000000067265766f6b650000000d7368613235363a746172676574000000046c6f73740000000d7368613235363a7369676e657200000014323032362d31302d30385430303a30303a30305a000000203030303130323033303430353036303730383039306130623063306430653066")
}

private func softwareP256BackendNeedsNoTouchID() throws {
    let backend = SoftwareP256Backend()
    let material = try backend.createKey(verificationPrompt: "ignored")
    let message = Data("software signer".utf8)
    let der = try backend.sign(keyRepresentation: material.representation, message: message, prompt: "ignored")
    let signature = try P256.Signing.ECDSASignature(derRepresentation: der)
    let publicKey = try P256.Signing.PublicKey(derRepresentation: material.publicKeyDER)
    try expect(publicKey.isValidSignature(signature, for: message))
}

private func decisionEngineSignsOnceAndBuildsExactSpawnAndIntentShapes() throws {
    let root = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: root) }
    let store = SignerStore(directory: root.appendingPathComponent("data"))
    let backend = SoftwareP256Backend()
    let material = try backend.createKey(verificationPrompt: "ignored")
    try store.writeNewKey(material.representation)
    let input = SigningInput(journalId: journalId, consumer: "approval", gateId: approvalGateId, title: "Deploy", requestDigest: approvalDigest, state: "pending", verificationStatus: "none", decisionSequence: nil, decision: nil, reason: nil)
    let client = FixedRhizomeClient(input: input)
    let instant = TimeFormat.parse("2026-10-09T00:00:00Z")!
    let engine = SignerEngine(backend: backend, store: store, client: client, now: { instant }, nonce: { "000102030405060708090a0b0c0d0e0f" })
    let result = try engine.signDecision(gateId: input.gateId, requested: "gate.approve", reason: "reviewed", correlationId: "cockpit:vector")

    try expect(backend.signPrompts == ["approval \(approvalGateId)\nDeploy\nallow: reviewed\n\(approvalDigest)"])
    let spawn = try JSONSerialization.jsonObject(with: result.spawn) as! [String: Any]
    let intent = try JSONSerialization.jsonObject(with: result.intent) as! [String: Any]
    try expect(Set(spawn.keys) == ["digest", "decision", "reason", "keyId", "signature"])
    try expect(spawn["decision"] as? String == "allow")
    try expect(Set(intent.keys) == ["kind", "gateId", "digest", "reason", "actor", "verification"])
    try expect(intent["kind"] as? String == "gate.approve")
    try expect(intent["actor"] as? String == "signer")

    let signatureObject = spawn["signature"] as! [String: Any]
    try expect(Set(signatureObject.keys) == ["keyId", "signedAt", "nonce", "sig"])
    let keyId = signatureObject["keyId"] as! String
    let signedAt = signatureObject["signedAt"] as! String
    let nonce = signatureObject["nonce"] as! String
    let signature = try P256.Signing.ECDSASignature(derRepresentation: Data(base64Encoded: signatureObject["sig"] as! String)!)
    let message = Canonical.decisionMessage(journalId: journalId, consumer: "approval", gateId: input.gateId, requestDigest: input.requestDigest, decision: "allow", reason: "reviewed", keyId: keyId, signedAt: signedAt, nonce: nonce)
    try expect(try P256.Signing.PublicKey(derRepresentation: material.publicKeyDER).isValidSignature(signature, for: message))
}

private func keyIDAndAnchorAreGoCompatibleAndByteExact() throws {
    let der = Data(base64Encoded: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEaxfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpZP40Li/hp/m47n60p8D54WK84zV2sxXs7LtkBoN79R9Q==")!
    try expect(Canonical.keyId(publicKeyDER: der) == "sha256:5cd252fb0ce8932436faf8ccd1040981b89ee4ad6b9fe9e2a2b7e71aacb27cd3")
    try expect(String(data: Canonical.anchorJSON(publicKeyDER: der), encoding: .utf8) == "{\"format\":\"rhizome-trust-anchor-v1\",\"principal\":\"H\",\"algorithm\":\"ecdsa-p256\",\"assurance\":\"key\",\"publicKey\":\"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEaxfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpZP40Li/hp/m47n60p8D54WK84zV2sxXs7LtkBoN79R9Q==\"}")
}

private func decisionMappingAndNotSignableCases() throws {
    try expect(try DecisionPolicy.map(consumer: "question", requested: "gate.requestChanges") == DecisionMapping(kind: "gate.requestChanges", decision: "requestChanges"))
    try expect(try DecisionPolicy.map(consumer: "approval", requested: "gate.approve") == DecisionMapping(kind: "gate.approve", decision: "allow"))
    try expect(try DecisionPolicy.map(consumer: "approval", requested: "deny") == DecisionMapping(kind: "gate.reject", decision: "deny"))
    try expectExit(.notSignable) { try DecisionPolicy.map(consumer: "approval", requested: "requestChanges") }
    try expectExit(.notSignable) { try DecisionPolicy.map(consumer: "question", requested: "allow") }
    let terminal = SigningInput(journalId: journalId, consumer: "question", gateId: "q-x", title: "x", requestDigest: "d", state: "approved", verificationStatus: "none", decisionSequence: 1, decision: "approve", reason: "")
    try expectExit(.notSignable) { try DecisionPolicy.requireSignableState(terminal) }
}

private func promptTextAndLimit() throws {
    let prompt = try PromptText.decision(consumer: "question", gateId: "q-1", title: "제목", decision: "reject", reason: "이유", requestDigest: "sha256:abc")
    try expect(prompt == "question q-1\n제목\nreject: 이유\nsha256:abc")
    let fixed = "question q-1\nt\napprove: \nsha256:x".unicodeScalars.count
    let fittingReason = String(repeating: "a", count: 600 - fixed)
    try expect(try PromptText.decision(consumer: "question", gateId: "q-1", title: "t", decision: "approve", reason: fittingReason, requestDigest: "sha256:x").unicodeScalars.count == 600)
    try expectExit(.refused) { try PromptText.decision(consumer: "question", gateId: "q-1", title: "t", decision: "approve", reason: fittingReason + "a", requestDigest: "sha256:x") }

    let combining = "a" + String(repeating: "\u{0301}", count: 600)
    try expect(combining.count == 1)
    try expectExit(.refused) { try PromptText.decision(consumer: "question", gateId: "q-1", title: "t", decision: "approve", reason: combining, requestDigest: "sha256:x") }
}

private func promptSanitizationRefusesEveryClassAndField() throws {
    let unsafe = ["\r", "\n", "\u{0085}", "\u{0001}", "\u{007f}", "\u{2028}", "\u{2029}", "\u{202d}", "\u{2067}", "\u{200b}", "\u{200e}", "\u{feff}", "\u{2062}"]
    for scalar in unsafe {
        try expectExit(.refused) {
            try PromptText.decision(consumer: "question", gateId: "q-1", title: "title", decision: "approve", reason: "bad\(scalar)value", requestDigest: "sha256:x")
        }
    }
    let fields = [
        ("question\u{200b}", "q-1", "title", "reason", "sha256:x"),
        ("question", "q-1\u{200b}", "title", "reason", "sha256:x"),
        ("question", "q-1", "title\u{200b}", "reason", "sha256:x"),
        ("question", "q-1", "title", "reason\u{200b}", "sha256:x"),
        ("question", "q-1", "title", "reason", "sha256:x\u{200b}"),
    ]
    for field in fields {
        try expectExit(.refused) {
            try PromptText.decision(consumer: field.0, gateId: field.1, title: field.2, decision: "approve", reason: field.3, requestDigest: field.4)
        }
    }
    try expectExit(.refused) { try PromptText.attest(count: 1, manifestDigest: "sha256:x\u{202e}") }
    try expect(try PromptText.attest(count: 1, manifestDigest: "sha256:x").split(separator: "\n", omittingEmptySubsequences: false).count == 2)
}

private func logDoesNotContainReasonOrTitleAndRateLimitPersists() throws {
    let directory = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: directory) }
    let url = directory.appendingPathComponent("requests.log")
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    let logger = RequestLogger(url: url, now: { now })
    let secretReason = "never log this reason"
    let secretTitle = "never log this title"
    let record = RequestLogRecord(consumer: "question", gateId: "q-1", decision: "approve", reasonHash16: RequestLogger.reasonHash16(secretReason), requestDigest: "sha256:d", correlationId: "cockpit:x", keyId: "sha256:k")
    for _ in 0..<6 { _ = try logger.performPrompt(record) { true } }
    var prompted = false
    try expectExit(.refused) { _ = try logger.performPrompt(record) { prompted = true; return true } }
    try expect(!prompted)
    let text = try String(contentsOf: url, encoding: .utf8)
    try expect(!text.contains(secretReason))
    try expect(!text.contains(secretTitle))
    try expect(text.contains("ratelimited"))
    try expect(RequestLogger.promptCount(in: Data(text.utf8), since: now.addingTimeInterval(-60)) == 6)
}

private func dataDirectoryAndReviewPermissions() throws {
    let directory = temporaryDirectory().appendingPathComponent("data", isDirectory: true)
    defer { try? FileManager.default.removeItem(at: directory.deletingLastPathComponent()) }
    let store = SignerStore(directory: directory)
    try store.ensureDirectory()
    let review = directory.appendingPathComponent("review.json")
    try store.writeReview(Data("{}".utf8), to: review)
    let directoryMode = (try FileManager.default.attributesOfItem(atPath: directory.path)[.posixPermissions] as? NSNumber)?.intValue
    let reviewMode = (try FileManager.default.attributesOfItem(atPath: review.path)[.posixPermissions] as? NSNumber)?.intValue
    try expect(directoryMode == 0o700)
    try expect(reviewMode == 0o600)
}

private func signStdinRejectsArguments() throws {
    try CommandLineDriver.validateSignStdinArguments(["sign-stdin"])
    try expectExit(.refused) { try CommandLineDriver.validateSignStdinArguments(["sign-stdin", "-rhizome", "http://127.0.0.1:1"]) }
}

private func loopbackURLRedirectAndProxyPolicy() throws {
    try expect(try RhizomeURLPolicy.validate("http://127.23.4.5:8790").host == "127.23.4.5")
    try expect(try RhizomeURLPolicy.validate("http://[::1]:8790").host == "::1")
    for value in ["http://localhost:8790", "http://10.0.0.1:8790", "https://127.0.0.1:8790", "http://127.0.0.1:8790/path"] {
        try expectExit(.refused) { try RhizomeURLPolicy.validate(value) }
    }
    try expect(!NoRedirectDelegate.followsRedirects)
    try expect(HTTPPolicy.configuration().connectionProxyDictionary?.count == 0)
    try expect(HTTPPolicy.configuration().timeoutIntervalForRequest == 10)
}

private func expectMissingAndMismatch() throws {
    try expectExit(.refused) { try AttestationPolicy.requireExpected(nil, actual: "sha256:a") }
    try expectExit(.refused, message: "manifest changed; recomputed digest: sha256:b") { try AttestationPolicy.requireExpected("sha256:a", actual: "sha256:b") }
    try AttestationPolicy.requireExpected("sha256:a", actual: "sha256:a")
}

private func items257TruncateToFirst256BySequence() throws {
    let items = (1...257).reversed().map { AttestItem(consumer: "question", gateId: "q-\($0)", decisionSequence: UInt64($0), digest: "d-\($0)", decision: "approve", reason: "") }
    let selection = AttestationPolicy.select(items)
    try expect(selection.total == 257)
    try expect(selection.items.count == 256)
    try expect(selection.items.first?.decisionSequence == 1)
    try expect(selection.items.last?.decisionSequence == 256)
}

private func spawnInputEndToEndStrictJSONPath() throws {
    let data = Data("{\"gateId\":\"\(questionGateId)\",\"kind\":\"gate.approve\",\"reason\":\"reviewed\",\"correlationId\":\"corr-1\"}".utf8)
    let input = try CommandLineDriver.spawnInput(data)
    try expect(input.gateId == questionGateId)
    try expect(input.kind == "gate.approve")
    try expect(input.reason == "reviewed")
    try expect(input.correlationId == "corr-1")
    try expectExit(.refused) { try CommandLineDriver.spawnInput(Data("{\"gateId\":\"x\",\"kind\":\"k\",\"reason\":\"r\",\"correlationId\":\"c\",\"extra\":1}".utf8)) }
    try expectExit(.refused) { try CommandLineDriver.spawnInput(Data("{\"gateId\":1,\"kind\":\"k\",\"reason\":\"r\",\"correlationId\":\"c\"}".utf8)) }
    try expectExit(.refused) { try CommandLineDriver.spawnInput(Data("{}{}".utf8)) }
}

private func strictDecoderStatusAndIntegerTypes() throws {
    let chainValid = Data("{\"consumer\":\"question\",\"gateId\":\"\(questionGateId)\",\"title\":\"t\",\"requestDigest\":\"\(questionDigest)\",\"state\":\"pending\",\"verificationStatus\":\"chain-valid\"}".utf8)
    let decoded = try StrictJSON.signingInput(chainValid)
    try expect(decoded.verificationStatus == "chain-valid")
    try expect(decoded.journalId == nil)

    for invalidNumber in ["true", "1.0", "1.5"] {
        let data = Data("{\"journalId\":\"\(journalId)\",\"consumer\":\"question\",\"gateId\":\"\(questionGateId)\",\"title\":\"t\",\"requestDigest\":\"\(questionDigest)\",\"state\":\"approved\",\"verificationStatus\":\"none\",\"decisionSequence\":\(invalidNumber),\"decision\":\"approve\",\"reason\":\"\"}".utf8)
        try expectExit(.rhizomeReadFailed) { try StrictJSON.signingInput(data) }
    }
    let integer = Data("{\"journalId\":\"\(journalId)\",\"consumer\":\"question\",\"gateId\":\"\(questionGateId)\",\"title\":\"t\",\"requestDigest\":\"\(questionDigest)\",\"state\":\"approved\",\"verificationStatus\":\"none\",\"decisionSequence\":1,\"decision\":\"approve\",\"reason\":\"\"}".utf8)
    try expect(try StrictJSON.signingInput(integer).decisionSequence == 1)
}

private func engineNotSignableAndAnchorlessCases() throws {
    let root = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: root) }
    let store = SignerStore(directory: root.appendingPathComponent("data"))
    let backend = SoftwareP256Backend()
    let material = try backend.createKey(verificationPrompt: "ignored")
    try store.writeNewKey(material.representation)

    let approval = SigningInput(journalId: journalId, consumer: "approval", gateId: approvalGateId, title: "Deploy", requestDigest: approvalDigest, state: "pending", verificationStatus: "none", decisionSequence: nil, decision: nil, reason: nil)
    let approvalEngine = SignerEngine(backend: backend, store: store, client: FixedRhizomeClient(input: approval))
    try expectExit(.notSignable) { try approvalEngine.signDecision(gateId: approvalGateId, requested: "requestChanges", reason: "revise") }
    try expectExit(.notSignable) { try approvalEngine.signDecision(gateId: approvalGateId, requested: "reject", reason: "") }

    let question = SigningInput(journalId: journalId, consumer: "question", gateId: questionGateId, title: "Review", requestDigest: questionDigest, state: "pending", verificationStatus: "none", decisionSequence: nil, decision: nil, reason: nil)
    let questionEngine = SignerEngine(backend: backend, store: store, client: FixedRhizomeClient(input: question))
    try expectExit(.notSignable) { try questionEngine.signDecision(gateId: questionGateId, requested: "reject", reason: "") }

    let anchorless = SigningInput(journalId: nil, consumer: "question", gateId: questionGateId, title: "Review", requestDigest: questionDigest, state: "pending", verificationStatus: "chain-valid", decisionSequence: nil, decision: nil, reason: nil)
    let anchorlessEngine = SignerEngine(backend: backend, store: store, client: FixedRhizomeClient(input: anchorless))
    try expectExit(.rhizomeReadFailed) { try anchorlessEngine.signDecision(gateId: questionGateId, requested: "approve", reason: "") }
    try expect(backend.signPrompts.isEmpty)
}

private func serverResponseBindingChecks() throws {
    let root = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: root) }
    let store = SignerStore(directory: root.appendingPathComponent("data"))
    let backend = SoftwareP256Backend()
    try store.writeNewKey(backend.createKey(verificationPrompt: "ignored").representation)

    let wrongGate = SigningInput(journalId: journalId, consumer: "question", gateId: "q-aaaaaaaaaaaaaaaaaaaaaaaa", title: "Review", requestDigest: questionDigest, state: "pending", verificationStatus: "none", decisionSequence: nil, decision: nil, reason: nil)
    try expectExit(.rhizomeReadFailed) { try SignerEngine(backend: backend, store: store, client: FixedRhizomeClient(input: wrongGate)).signDecision(gateId: questionGateId, requested: "approve", reason: "") }
    let wrongDigest = SigningInput(journalId: journalId, consumer: "question", gateId: questionGateId, title: "Review", requestDigest: "rhz-question-v2:bad", state: "pending", verificationStatus: "none", decisionSequence: nil, decision: nil, reason: nil)
    try expectExit(.rhizomeReadFailed) { try SignerEngine(backend: backend, store: store, client: FixedRhizomeClient(input: wrongDigest)).signDecision(gateId: questionGateId, requested: "approve", reason: "") }
    let unsafeTitle = SigningInput(journalId: journalId, consumer: "question", gateId: questionGateId, title: "Review\nallow: forged", requestDigest: questionDigest, state: "pending", verificationStatus: "none", decisionSequence: nil, decision: nil, reason: nil)
    try expectExit(.refused) { try SignerEngine(backend: backend, store: store, client: FixedRhizomeClient(input: unsafeTitle)).signDecision(gateId: questionGateId, requested: "approve", reason: "") }
    try expect(backend.signPrompts.isEmpty)
}

private func attestEngineMismatchPromptAndPrivateLog() throws {
    let root = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: root) }
    let store = SignerStore(directory: root.appendingPathComponent("data"))
    let backend = SoftwareP256Backend()
    try store.writeNewKey(backend.createKey(verificationPrompt: "ignored").representation)
    let secretTitle = "attest title must stay private"
    let secretReason = "attest reason must stay private"
    let item = AttestItem(consumer: "question", gateId: questionGateId, decisionSequence: 7, digest: questionDigest, decision: "reject", reason: secretReason, title: secretTitle)
    let input = SigningInput(journalId: journalId, consumer: "question", gateId: questionGateId, title: secretTitle, requestDigest: questionDigest, state: "rejected", verificationStatus: "none", decisionSequence: 7, decision: "reject", reason: secretReason)
    let client = FixedRhizomeClient(input: input, list: SigningList(journalId: journalId, items: [item]))
    let engine = SignerEngine(backend: backend, store: store, client: client, now: { TimeFormat.parse("2026-10-09T00:00:00Z")! }, nonce: { "000102030405060708090a0b0c0d0e0f" })
    let digest = Canonical.manifestDigest([item])
    try expectExit(.refused, message: "manifest changed; recomputed digest: \(digest)") { try engine.signAttestation(expected: "sha256:wrong") }
    try expect(backend.signPrompts.isEmpty)
    _ = try engine.signAttestation(expected: digest)
    try expect(backend.signPrompts.count == 1)
    try expect(backend.signPrompts[0].split(separator: "\n", omittingEmptySubsequences: false).count == 2)
    let log = try String(contentsOf: store.logURL, encoding: .utf8)
    try expect(!log.contains(secretTitle))
    try expect(!log.contains(secretReason))
    try expect(log.contains(questionGateId))
}

private func stdinCapAndSignerDirectoryPolicy() throws {
    let root = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: root) }
    let oversized = root.appendingPathComponent("oversized")
    try Data(repeating: 0x61, count: CommandLineDriver.maximumStdinBytes + 1).write(to: oversized)
    let handle = try FileHandle(forReadingFrom: oversized)
    defer { try? handle.close() }
    try expectExit(.refused, message: "sign-stdin request exceeds 64 KiB") { try CommandLineDriver.readSignStdin(handle) }
    try expectExit(.refused, message: "sign-stdin request exceeds 64 KiB") { try CommandLineDriver.run(arguments: ["sign-stdin"], stdin: Data(repeating: 0x61, count: CommandLineDriver.maximumStdinBytes + 1)) }

    let custom = root.appendingPathComponent("custom")
    let environment = ["SIGNER_DIR": custom.path]
    try expect(SignerStore(environment: environment).directory.path == custom.path)
    try expect(SignerStore(environment: environment, honorsEnvironment: false).directory.path == SignerStore.defaultDirectory.path)
}

private func storageRejectsUnsafeDirectoryAndKeySymlink() throws {
    let root = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: root) }
    let wrongMode = root.appendingPathComponent("wrong-mode")
    try FileManager.default.createDirectory(at: wrongMode, withIntermediateDirectories: false)
    _ = chmod(wrongMode.path, 0o755)
    try expectExit(.failure) { try SignerStore(directory: wrongMode).ensureDirectory() }

    let directory = root.appendingPathComponent("safe")
    let store = SignerStore(directory: directory)
    try store.ensureDirectory()
    let target = root.appendingPathComponent("target")
    try Data("not a key".utf8).write(to: target)
    try FileManager.default.createSymbolicLink(at: store.keyURL, withDestinationURL: target)
    try expectExit(.noKey) { try store.readKey() }
}

private func rejectionReasonIsSurfaced() throws {
    let input = SigningInput(journalId: journalId, consumer: "question", gateId: questionGateId, title: "Review", requestDigest: questionDigest, state: "pending", verificationStatus: "none", decisionSequence: nil, decision: nil, reason: nil)
    let client = FixedRhizomeClient(input: input, submitResponse: Data("{\"Accepted\":false,\"Reason\":\"stale manifest\"}".utf8))
    let root = temporaryDirectory()
    defer { try? FileManager.default.removeItem(at: root) }
    let engine = SignerEngine(backend: SoftwareP256Backend(), store: SignerStore(directory: root.appendingPathComponent("data")), client: client)
    try expectExit(.rhizomeReadFailed, message: "Rhizome refused intent: stale manifest") { try engine.submit(Data("{}".utf8)) }
}

private func vectorN1() -> [AttestItem] {
    [AttestItem(consumer: "question", gateId: "q-92caa77d4e8cb1f9a861ff35", decisionSequence: 7, digest: "rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61", decision: "approve", reason: "")]
}

private func temporaryDirectory() -> URL {
    let url = FileManager.default.temporaryDirectory.appendingPathComponent("rhizome-signer-tests-\(UUID().uuidString)", isDirectory: true)
    try! FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    _ = chmod(url.path, 0o700)
    return url
}

private func expectExit<T>(_ expected: SignerExit, message: String? = nil, _ body: () throws -> T) throws {
    do {
        _ = try body()
        throw AssertionFailure(message: "expected SignerFailure")
    } catch let failure as SignerFailure {
        try expect(failure.exit == expected, "expected exit \(expected.rawValue), got \(failure.exit.rawValue)")
        if let message { try expect(failure.message == message, "expected message '\(message)', got '\(failure.message)'") }
    } catch let failure as AssertionFailure {
        throw failure
    } catch {
        throw AssertionFailure(message: "unexpected error: \(error)")
    }
}

private final class SoftwareP256Backend: SigningBackend {
    private let key: P256.Signing.PrivateKey
    var signPrompts: [String] = []

    init() {
        var raw = Data(repeating: 0, count: 32)
        raw[31] = 1
        key = try! P256.Signing.PrivateKey(rawRepresentation: raw)
    }

    func createKey(verificationPrompt: String) throws -> KeyMaterial {
        KeyMaterial(representation: key.rawRepresentation, publicKeyDER: key.publicKey.derRepresentation)
    }

    func publicKeyDER(keyRepresentation: Data) throws -> Data {
        try P256.Signing.PrivateKey(rawRepresentation: keyRepresentation).publicKey.derRepresentation
    }

    func sign(keyRepresentation: Data, message: Data, prompt: String) throws -> Data {
        signPrompts.append(prompt)
        return try P256.Signing.PrivateKey(rawRepresentation: keyRepresentation).signature(for: message).derRepresentation
    }
}

private final class FixedRhizomeClient: RhizomeClientProtocol {
    let input: SigningInput
    let list: SigningList
    let submitResponse: Data

    init(input: SigningInput, list: SigningList? = nil, submitResponse: Data = Data("{\"Accepted\":true}".utf8)) {
        self.input = input
        self.list = list ?? SigningList(journalId: input.journalId, items: [])
        self.submitResponse = submitResponse
    }

    func signingInput(gateId: String) throws -> SigningInput { input }
    func unverifiedInputs() throws -> SigningList { list }
    func submit(intent: Data) throws -> Data { submitResponse }
}

private let cases: [(String, () throws -> Void)] = [
    ("decisionMessageMatchesGoVector", decisionMessageMatchesGoVector),
    ("attestN1AndN3GoGeneratedFixtures", attestN1AndN3GoGeneratedFixtures),
    ("addAndRevokeGoGeneratedFixtures", addAndRevokeGoGeneratedFixtures),
    ("softwareP256BackendNeedsNoTouchID", softwareP256BackendNeedsNoTouchID),
    ("decisionEngineSignsOnceAndBuildsExactSpawnAndIntentShapes", decisionEngineSignsOnceAndBuildsExactSpawnAndIntentShapes),
    ("keyIDAndAnchorAreGoCompatibleAndByteExact", keyIDAndAnchorAreGoCompatibleAndByteExact),
    ("decisionMappingAndNotSignableCases", decisionMappingAndNotSignableCases),
    ("promptTextAndLimit", promptTextAndLimit),
    ("promptSanitizationRefusesEveryClassAndField", promptSanitizationRefusesEveryClassAndField),
    ("logDoesNotContainReasonOrTitleAndRateLimitPersists", logDoesNotContainReasonOrTitleAndRateLimitPersists),
    ("dataDirectoryAndReviewPermissions", dataDirectoryAndReviewPermissions),
    ("signStdinRejectsArguments", signStdinRejectsArguments),
    ("loopbackURLRedirectAndProxyPolicy", loopbackURLRedirectAndProxyPolicy),
    ("expectMissingAndMismatch", expectMissingAndMismatch),
    ("items257TruncateToFirst256BySequence", items257TruncateToFirst256BySequence),
    ("spawnInputEndToEndStrictJSONPath", spawnInputEndToEndStrictJSONPath),
    ("strictDecoderStatusAndIntegerTypes", strictDecoderStatusAndIntegerTypes),
    ("engineNotSignableAndAnchorlessCases", engineNotSignableAndAnchorlessCases),
    ("serverResponseBindingChecks", serverResponseBindingChecks),
    ("attestEngineMismatchPromptAndPrivateLog", attestEngineMismatchPromptAndPrivateLog),
    ("stdinCapAndSignerDirectoryPolicy", stdinCapAndSignerDirectoryPolicy),
    ("storageRejectsUnsafeDirectoryAndKeySymlink", storageRejectsUnsafeDirectoryAndKeySymlink),
    ("rejectionReasonIsSurfaced", rejectionReasonIsSurfaced),
]

var passed = 0
var failed = 0
for (name, body) in cases {
    do {
        try body()
        passed += 1
        print("PASS \(name)")
    } catch {
        failed += 1
        print("FAIL \(name): \(error)")
    }
}
print("selftest: \(passed) passed, \(failed) failed")
if failed != 0 { exit(EXIT_FAILURE) }
