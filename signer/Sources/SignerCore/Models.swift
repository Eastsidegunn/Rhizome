import Foundation

public enum SignerExit: Int32 {
    case success = 0
    case failure = 1
    case refused = 2
    case rhizomeReadFailed = 3
    case userCancelled = 4
    case noKey = 5
    case notSignable = 6
}

public struct SignerFailure: Error, CustomStringConvertible {
    public let exit: SignerExit
    public let message: String

    public init(_ exit: SignerExit, _ message: String) {
        self.exit = exit
        self.message = message
    }

    public var description: String { message }
}

public struct SignatureEnvelope: Codable, Equatable {
    public let keyId: String
    public let signedAt: String
    public let nonce: String
    public let sig: String

    public init(keyId: String, signedAt: String, nonce: String, sig: String) {
        self.keyId = keyId
        self.signedAt = signedAt
        self.nonce = nonce
        self.sig = sig
    }
}

public struct SigningInput: Equatable {
    public let journalId: String?
    public let consumer: String
    public let gateId: String
    public let title: String
    public let requestDigest: String
    public let state: String
    public let verificationStatus: String
    public let decisionSequence: UInt64?
    public let decision: String?
    public let reason: String?

    public init(journalId: String?, consumer: String, gateId: String, title: String, requestDigest: String, state: String, verificationStatus: String, decisionSequence: UInt64?, decision: String?, reason: String?) {
        self.journalId = journalId
        self.consumer = consumer
        self.gateId = gateId
        self.title = title
        self.requestDigest = requestDigest
        self.state = state
        self.verificationStatus = verificationStatus
        self.decisionSequence = decisionSequence
        self.decision = decision
        self.reason = reason
    }
}

public struct AttestItem: Codable, Equatable {
    public let consumer: String
    public let gateId: String
    public let decisionSequence: UInt64
    public let digest: String
    public let decision: String
    public let reason: String
    public let title: String

    public init(consumer: String, gateId: String, decisionSequence: UInt64, digest: String, decision: String, reason: String, title: String = "") {
        self.consumer = consumer
        self.gateId = gateId
        self.decisionSequence = decisionSequence
        self.digest = digest
        self.decision = decision
        self.reason = reason
        self.title = title
    }
}

public struct SigningList: Equatable {
    public let journalId: String?
    public let items: [AttestItem]

    public init(journalId: String?, items: [AttestItem]) {
        self.journalId = journalId
        self.items = items
    }
}

public struct DecisionMapping: Equatable {
    public let kind: String
    public let decision: String

    public init(kind: String, decision: String) {
        self.kind = kind
        self.decision = decision
    }
}

public enum DecisionPolicy {
    public static func map(consumer: String, requested: String) throws -> DecisionMapping {
        let action: String
        switch requested {
        case "gate.approve": action = "approve"
        case "gate.reject": action = "reject"
        case "gate.requestChanges": action = "requestChanges"
        default: action = requested
        }
        switch (consumer, action) {
        case ("question", "approve"):
            return DecisionMapping(kind: "gate.approve", decision: "approve")
        case ("question", "reject"):
            return DecisionMapping(kind: "gate.reject", decision: "reject")
        case ("question", "requestChanges"):
            return DecisionMapping(kind: "gate.requestChanges", decision: "requestChanges")
        case ("approval", "approve"), ("approval", "allow"):
            return DecisionMapping(kind: "gate.approve", decision: "allow")
        case ("approval", "reject"), ("approval", "deny"):
            return DecisionMapping(kind: "gate.reject", decision: "deny")
        default:
            throw SignerFailure(.notSignable, "decision is not signable for this gate")
        }
    }

    public static func requireSignableState(_ input: SigningInput) throws {
        switch (input.consumer, input.state) {
        case ("question", "pending"), ("question", "changes_requested"), ("approval", "pending"):
            return
        default:
            throw SignerFailure(.notSignable, "gate is not in a signable state")
        }
    }

    public static func requireReason(decision: String, reason: String) throws {
        if (decision == "reject" || decision == "deny" || decision == "requestChanges") && reason.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            throw SignerFailure(.notSignable, "this decision requires a reason")
        }
    }
}

public enum PromptText {
    public static let maximumCharacters = 600

    public static func validateField(_ value: String, name: String) throws {
        for scalar in value.unicodeScalars {
            let code = scalar.value
            if code <= 0x1f || (0x7f...0x9f).contains(code) ||
                (0x200b...0x200f).contains(code) ||
                (0x2028...0x202e).contains(code) ||
                (0x2060...0x2069).contains(code) || code == 0xfeff {
                throw SignerFailure(.refused, "unsafe character in \(name)")
            }
        }
    }

    public static func requireWithinLimit(_ value: String) throws {
        guard value.unicodeScalars.count <= maximumCharacters else {
            throw SignerFailure(.refused, "signing prompt exceeds 600 unicode scalars")
        }
    }

    public static func decision(consumer: String, gateId: String, title: String, decision: String, reason: String, requestDigest: String) throws -> String {
        for (name, field) in [("consumer", consumer), ("gateId", gateId), ("title", title), ("reason", reason), ("requestDigest", requestDigest)] {
            try validateField(field, name: name)
        }
        let value = "\(consumer) \(gateId)\n\(title)\n\(decision): \(reason)\n\(requestDigest)"
        guard value.split(separator: "\n", omittingEmptySubsequences: false).count == 4 else {
            throw SignerFailure(.refused, "invalid decision prompt line count")
        }
        try requireWithinLimit(value)
        return value
    }

    public static func attest(count: Int, manifestDigest: String) throws -> String {
        try validateField(manifestDigest, name: "manifestDigest")
        let value = "사후 확인 \(count)건\n\(manifestDigest)"
        guard value.split(separator: "\n", omittingEmptySubsequences: false).count == 2 else {
            throw SignerFailure(.refused, "invalid attestation prompt line count")
        }
        try requireWithinLimit(value)
        return value
    }
}

public enum SigningFieldPolicy {
    private static let lowerHex = Set("0123456789abcdef")

    public static func validGateId(_ value: String, consumer: String? = nil) -> Bool {
        let prefix: String
        switch consumer {
        case "question": prefix = "q-"
        case "approval": prefix = "appr-"
        case nil:
            return validGateId(value, consumer: "question") || validGateId(value, consumer: "approval")
        default:
            return false
        }
        guard value.hasPrefix(prefix) else { return false }
        let suffix = value.dropFirst(prefix.count)
        return suffix.count == 24 && suffix.allSatisfy { lowerHex.contains($0) }
    }

    public static func validRequestDigest(_ value: String, consumer: String) -> Bool {
        switch consumer {
        case "question":
            let prefixes = ["rhz-question-v1:", "rhz-question-v2:"]
            guard let prefix = prefixes.first(where: value.hasPrefix) else { return false }
            let suffix = value.dropFirst(prefix.count)
            return suffix.count == 64 && suffix.allSatisfy { lowerHex.contains($0) }
        case "approval":
            let prefix = "hx-args-digest-v1:"
            return value.hasPrefix(prefix) && value.count > prefix.count
        default:
            return false
        }
    }
}

public enum AttestationPolicy {
    public static let maximumItems = 256

    public static func select(_ items: [AttestItem]) -> (items: [AttestItem], total: Int) {
        let sorted = items.sorted {
            if $0.decisionSequence != $1.decisionSequence { return $0.decisionSequence < $1.decisionSequence }
            return $0.gateId < $1.gateId
        }
        return (Array(sorted.prefix(maximumItems)), sorted.count)
    }

    public static func requireExpected(_ expected: String?, actual: String) throws {
        guard let expected, !expected.isEmpty else {
            throw SignerFailure(.refused, "-expect is required")
        }
        guard expected == actual else {
            throw SignerFailure(.refused, "manifest changed; recomputed digest: \(actual)")
        }
    }
}
