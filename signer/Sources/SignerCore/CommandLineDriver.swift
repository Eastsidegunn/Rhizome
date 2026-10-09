import Foundation

public enum CommandLineDriver {
    public static let version = "rhizome-signer 1"
    public static let maximumStdinBytes = 64 * 1024

    public static func validateSignStdinArguments(_ arguments: [String]) throws {
        guard arguments == ["sign-stdin"] else { throw SignerFailure(.refused, "sign-stdin accepts no arguments") }
    }

    public static func readSignStdin(_ handle: FileHandle) throws -> Data {
        var result = Data()
        while result.count <= maximumStdinBytes {
            let remaining = maximumStdinBytes + 1 - result.count
            let chunk: Data
            do { chunk = try handle.read(upToCount: min(8192, remaining)) ?? Data() }
            catch { throw SignerFailure(.refused, "cannot read sign-stdin request") }
            if chunk.isEmpty { return result }
            result.append(chunk)
        }
        throw SignerFailure(.refused, "sign-stdin request exceeds 64 KiB")
    }

    public static func run(arguments: [String], stdin: Data = Data()) throws -> Data {
        guard let command = arguments.first else { throw usage() }
        if command == "version" {
            guard arguments.count == 1 else { throw usage() }
            return line(Data(version.utf8))
        }
        if command == "sign-stdin" {
            try validateSignStdinArguments(arguments)
            guard stdin.count <= maximumStdinBytes else { throw SignerFailure(.refused, "sign-stdin request exceeds 64 KiB") }
            let input = try spawnInput(stdin)
            let engine = try makeEngine(urlString: RhizomeURLPolicy.defaultURL, honorsEnvironment: false)
            return line(try engine.signDecision(gateId: input.gateId, requested: input.kind, reason: input.reason, correlationId: input.correlationId).spawn)
        }
        if command == "key" { return try runKey(Array(arguments.dropFirst())) }
        if command == "sign" { return try runSign(Array(arguments.dropFirst())) }
        if command == "attest" { return try runAttest(Array(arguments.dropFirst())) }
        throw usage()
    }

    private static func runKey(_ arguments: [String]) throws -> Data {
        guard let command = arguments.first else { throw usage() }
        switch command {
        case "init":
            guard arguments.count == 1 else { throw usage() }
            let result = try makeEngine(urlString: RhizomeURLPolicy.defaultURL).initializeKey()
            return Data(String(data: result.anchor, encoding: .utf8)!.appending("\nkeyId=\(result.keyId)\n").utf8)
        case "show":
            guard arguments.count == 1 || arguments == ["show", "-anchor"] else { throw usage() }
            let result = try makeEngine(urlString: RhizomeURLPolicy.defaultURL).showKey()
            if arguments.count == 2 { return line(result.anchor) }
            return Data(String(data: result.anchor, encoding: .utf8)!.appending("\nkeyId=\(result.keyId)\npublicKey=\(result.publicKey)\n").utf8)
        case "add":
            return try runKeyAdd(Array(arguments.dropFirst()))
        case "revoke":
            return try runKeyRevoke(Array(arguments.dropFirst()))
        default:
            throw usage()
        }
    }

    private static func runSign(_ arguments: [String]) throws -> Data {
        guard arguments.count >= 2 else { throw usage() }
        let gateId = arguments[0]
        let requested = arguments[1]
        var reason = ""
        var submit = false
        var url = RhizomeURLPolicy.defaultURL
        var index = 2
        while index < arguments.count {
            switch arguments[index] {
            case "-reason":
                index += 1; guard index < arguments.count else { throw usage() }; reason = arguments[index]
            case "-submit": submit = true
            case "-rhizome":
                index += 1; guard index < arguments.count else { throw usage() }; url = arguments[index]
            default: throw usage()
            }
            index += 1
        }
        let engine = try makeEngine(urlString: url)
        let result = try engine.signDecision(gateId: gateId, requested: requested, reason: reason)
        return line(submit ? try engine.submit(result.intent) : result.intent)
    }

    private static func runAttest(_ arguments: [String]) throws -> Data {
        var listMode = false
        var all = false
        var out: String?
        var expected: String?
        var url = RhizomeURLPolicy.defaultURL
        var index = 0
        while index < arguments.count {
            switch arguments[index] {
            case "-list": listMode = true
            case "-all": all = true
            case "-out":
                index += 1; guard index < arguments.count else { throw usage() }; out = arguments[index]
            case "-expect":
                index += 1; guard index < arguments.count else { throw usage() }; expected = arguments[index]
            case "-rhizome":
                index += 1; guard index < arguments.count else { throw usage() }; url = arguments[index]
            default: throw usage()
            }
            index += 1
        }
        let engine = try makeEngine(urlString: url)
        if listMode {
            guard !all, expected == nil, let out else { throw usage() }
            let plan = try engine.attestList()
            try engine.writeAttestReview(items: plan.items, total: plan.total, digest: plan.digest, to: URL(fileURLWithPath: out))
            return Data(attestSummary(items: plan.items, total: plan.total, digest: plan.digest).utf8)
        }
        guard out == nil, all else { throw usage() }
        try AttestationPolicy.requireExpected(expected, actual: expected ?? "")
        let intent = try engine.signAttestation(expected: expected)
        return line(try engine.submit(intent))
    }

    private static func runKeyAdd(_ arguments: [String]) throws -> Data {
        guard let path = arguments.first else { throw usage() }
        var principal: String?
        var submit = false
        var url = RhizomeURLPolicy.defaultURL
        var index = 1
        while index < arguments.count {
            switch arguments[index] {
            case "-principal": index += 1; guard index < arguments.count else { throw usage() }; principal = arguments[index]
            case "-submit": submit = true
            case "-rhizome": index += 1; guard index < arguments.count else { throw usage() }; url = arguments[index]
            default: throw usage()
            }
            index += 1
        }
        guard let principal else { throw usage() }
        let der = try Data(contentsOf: URL(fileURLWithPath: path))
        let engine = try makeEngine(urlString: url)
        let intent = try engine.addKey(publicKeyDER: der, principal: principal)
        return line(submit ? try engine.submit(intent) : intent)
    }

    private static func runKeyRevoke(_ arguments: [String]) throws -> Data {
        guard let keyId = arguments.first else { throw usage() }
        var reason: String?
        var submit = false
        var url = RhizomeURLPolicy.defaultURL
        var index = 1
        while index < arguments.count {
            switch arguments[index] {
            case "-reason": index += 1; guard index < arguments.count else { throw usage() }; reason = arguments[index]
            case "-submit": submit = true
            case "-rhizome": index += 1; guard index < arguments.count else { throw usage() }; url = arguments[index]
            default: throw usage()
            }
            index += 1
        }
        guard let reason else { throw usage() }
        let engine = try makeEngine(urlString: url)
        let intent = try engine.revokeKey(keyId: keyId, reason: reason)
        return line(submit ? try engine.submit(intent) : intent)
    }

    private static func makeEngine(urlString: String, honorsEnvironment: Bool = true) throws -> SignerEngine {
        let url = try RhizomeURLPolicy.validate(urlString)
        let store = SignerStore(honorsEnvironment: honorsEnvironment)
        return SignerEngine(backend: SecureEnclaveBackend(), store: store, client: RhizomeClient(baseURL: url))
    }

    public static func spawnInput(_ data: Data) throws -> SpawnInput {
        let object: [String: Any]
        do { object = try StrictJSON.oneObject(data, exactKeys: ["gateId", "kind", "reason", "correlationId"]) }
        catch { throw SignerFailure(.refused, "sign-stdin requires one exact JSON object") }
        guard let gateId = object["gateId"] as? String,
              let kind = object["kind"] as? String,
              let reason = object["reason"] as? String,
              let correlationId = object["correlationId"] as? String else {
            throw SignerFailure(.refused, "invalid sign-stdin request")
        }
        return SpawnInput(gateId: gateId, kind: kind, reason: reason, correlationId: correlationId)
    }

    private static func attestSummary(items: [AttestItem], total: Int, digest: String) -> String {
        var lines = ["manifestDigest=\(digest)", "count=\(items.count)"]
        if total > items.count { lines.append("\(total)건 중 \(items.count)건, 나머지는 다음 회차") }
        lines += items.map { "gateId=\($0.gateId) decisionSequence=\($0.decisionSequence) decision=\($0.decision) requestDigest=\($0.digest) title=\($0.title)" }
        return lines.joined(separator: "\n") + "\n"
    }

    private static func line(_ data: Data) -> Data {
        var result = data
        if result.last != 0x0a { result.append(0x0a) }
        return result
    }

    private static func usage() -> SignerFailure {
        SignerFailure(.refused, "usage: rhizome-signer version | key ... | sign ... | sign-stdin | attest ...")
    }
}

public struct SpawnInput {
    public let gateId, kind, reason, correlationId: String

    public init(gateId: String, kind: String, reason: String, correlationId: String) {
        self.gateId = gateId
        self.kind = kind
        self.reason = reason
        self.correlationId = correlationId
    }
}
