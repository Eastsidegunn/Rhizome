import CryptoKit
import Foundation
import LocalAuthentication
import Security

public struct KeyMaterial {
    public let representation: Data
    public let publicKeyDER: Data

    public init(representation: Data, publicKeyDER: Data) {
        self.representation = representation
        self.publicKeyDER = publicKeyDER
    }
}

public protocol SigningBackend {
    func createKey(verificationPrompt: String) throws -> KeyMaterial
    func publicKeyDER(keyRepresentation: Data) throws -> Data
    func sign(keyRepresentation: Data, message: Data, prompt: String) throws -> Data
}

public struct SecureEnclaveBackend: SigningBackend {
    private static let verificationMessage = Data("rhizome-signer-key-verification-v1".utf8)

    public init() {}

    public func createKey(verificationPrompt: String) throws -> KeyMaterial {
        guard SecureEnclave.isAvailable else {
            throw SignerFailure(.failure, "Secure Enclave is unavailable")
        }
        var accessError: Unmanaged<CFError>?
        guard let access = SecAccessControlCreateWithFlags(
            kCFAllocatorDefault,
            kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
            [.privateKeyUsage, .userPresence],
            &accessError
        ) else {
            throw accessError?.takeRetainedValue() ?? SignerFailure(.failure, "cannot create key access control") as Error
        }
        let context = LAContext()
        context.localizedReason = verificationPrompt
        do {
            let key = try SecureEnclave.P256.Signing.PrivateKey(accessControl: access, authenticationContext: context)
            let signature = try key.signature(for: Self.verificationMessage)
            guard key.publicKey.isValidSignature(signature, for: Self.verificationMessage) else {
                throw SignerFailure(.failure, "key verification failed")
            }
            return KeyMaterial(representation: key.dataRepresentation, publicKeyDER: key.publicKey.derRepresentation)
        } catch {
            throw mapAuthenticationError(error)
        }
    }

    public func publicKeyDER(keyRepresentation: Data) throws -> Data {
        do {
            return try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: keyRepresentation).publicKey.derRepresentation
        } catch {
            throw SignerFailure(.noKey, "stored Secure Enclave key is unavailable")
        }
    }

    public func sign(keyRepresentation: Data, message: Data, prompt: String) throws -> Data {
        let context = LAContext()
        context.localizedReason = prompt
        do {
            let key = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: keyRepresentation, authenticationContext: context)
            return try key.signature(for: message).derRepresentation
        } catch {
            throw mapAuthenticationError(error)
        }
    }
}

private func mapAuthenticationError(_ error: Error) -> Error {
    let ns = error as NSError
    let cancellationCodes: Set<Int> = [
        LAError.userCancel.rawValue,
        LAError.appCancel.rawValue,
        LAError.systemCancel.rawValue,
        LAError.userFallback.rawValue,
        Int(errSecUserCanceled),
    ]
    if ns.domain == LAError.errorDomain && cancellationCodes.contains(ns.code) {
        return SignerFailure(.userCancelled, "authentication cancelled")
    }
    if cancellationCodes.contains(ns.code) {
        return SignerFailure(.userCancelled, "authentication cancelled")
    }
    return error
}

public enum PublicKeyFile {
    public static func inspect(der: Data) throws -> (algorithm: String, keyId: String) {
        if (try? P256.Signing.PublicKey(derRepresentation: der)) != nil {
            return ("ecdsa-p256", Canonical.keyId(publicKeyDER: der))
        }
        let ed25519Prefix = Data(hex: "302a300506032b6570032100")!
        if der.count == ed25519Prefix.count + 32, der.starts(with: ed25519Prefix) {
            let raw = der.dropFirst(ed25519Prefix.count)
            guard (try? Curve25519.Signing.PublicKey(rawRepresentation: raw)) != nil else {
                throw SignerFailure(.refused, "invalid public key DER")
            }
            return ("ed25519", Canonical.keyId(publicKeyDER: der))
        }
        throw SignerFailure(.refused, "public key must be PKIX DER P-256 or Ed25519")
    }
}
