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
        if (decision == "reject" || decision == "requestChanges") && reason.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            throw SignerFailure(.notSignable, "this decision requires a reason")
        }
    }
}

public enum PromptText {
    public static let maximumCharacters = 600

    public static func decision(consumer: String, gateId: String, title: String, decision: String, reason: String, requestDigest: String) throws -> String {
        let value = "\(consumer) \(gateId)\n\(title)\n\(decision): \(reason)\n\(requestDigest)"
        guard value.count <= maximumCharacters else {
            throw SignerFailure(.refused, "signing prompt exceeds 600 characters")
        }
        return value
    }

    public static func attest(count: Int, manifestDigest: String) -> String {
        "사후 확인 \(count)건\n\(manifestDigest)"
    }
}

public enum AttestationPolicy {
    public static let maximumItems = 256

    public static func select(_ items: [AttestItem], gateIds: [String]? = nil) throws -> (items: [AttestItem], total: Int) {
        let sorted = items.sorted {
            if $0.decisionSequence != $1.decisionSequence { return $0.decisionSequence < $1.decisionSequence }
            return $0.gateId < $1.gateId
        }
        if let gateIds {
            guard !gateIds.isEmpty, gateIds.count <= maximumItems else {
                throw SignerFailure(.refused, "attest selection must contain 1...256 gates")
            }
            let wanted = Set(gateIds)
            guard wanted.count == gateIds.count else {
                throw SignerFailure(.refused, "duplicate attest gate")
            }
            let selected = sorted.filter { wanted.contains($0.gateId) }
            guard selected.count == wanted.count else {
                throw SignerFailure(.notSignable, "attest gate is not available")
            }
            return (selected, selected.count)
        }
        return (Array(sorted.prefix(maximumItems)), sorted.count)
    }

    public static func requireExpected(_ expected: String?, actual: String) throws {
        guard let expected, !expected.isEmpty else {
            throw SignerFailure(.refused, "-expect is required")
        }
        guard expected == actual else {
            throw SignerFailure(.refused, "manifest changed")
        }
    }
}
