import Darwin
import Foundation
import Security

public final class SignerStore {
    public let directory: URL
    public var keyURL: URL { directory.appendingPathComponent("key.sep") }
    public var anchorURL: URL { directory.appendingPathComponent("anchor.json") }
    public var logURL: URL { directory.appendingPathComponent("requests.log") }

    public init(directory: URL? = nil, environment: [String: String] = ProcessInfo.processInfo.environment, honorsEnvironment: Bool = true) {
        if let directory {
            self.directory = directory
        } else if honorsEnvironment, let configured = environment["SIGNER_DIR"], !configured.isEmpty {
            self.directory = URL(fileURLWithPath: configured, isDirectory: true)
        } else {
            self.directory = Self.defaultDirectory
        }
    }

    public static var defaultDirectory: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/rhizome-signer", isDirectory: true)
    }

    public func ensureDirectory() throws {
        try Self.ensureProtectedDirectory(directory)
    }

    public func readKey() throws -> Data {
        var directoryInfo = stat()
        guard lstat(directory.path, &directoryInfo) == 0 else {
            if errno == ENOENT { throw SignerFailure(.noKey, "no signing key; run key init") }
            throw posixFailure("cannot inspect signer directory")
        }
        try Self.verifyProtectedDirectory(directory, info: directoryInfo)
        let fd = open(keyURL.path, O_RDONLY | O_NOFOLLOW)
        guard fd >= 0 else {
            if errno == ENOENT { throw SignerFailure(.noKey, "no signing key; run key init") }
            throw SignerFailure(.noKey, "cannot read signing key")
        }
        defer { close(fd) }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while true {
            let amount = read(fd, &buffer, buffer.count)
            if amount < 0 {
                if errno == EINTR { continue }
                throw SignerFailure(.noKey, "cannot read signing key")
            }
            if amount == 0 { break }
            data.append(buffer, count: amount)
        }
        return data
    }

    public func writeInitialKey(_ key: Data, anchor: Data) throws {
        try ensureDirectory()
        guard !pathExists(keyURL), !pathExists(anchorURL) else {
            throw SignerFailure(.refused, "signing key already exists")
        }
        try writeProtected(anchor, to: anchorURL, exclusive: true)
        do {
            try writeProtected(key, to: keyURL, exclusive: true)
        } catch {
            _ = unlink(anchorURL.path)
            _ = unlink(keyURL.path)
            throw error
        }
    }

    public func writeNewKey(_ data: Data) throws {
        try ensureDirectory()
        guard !pathExists(keyURL), !pathExists(anchorURL) else {
            throw SignerFailure(.refused, "signing key already exists")
        }
        try writeProtected(data, to: keyURL, exclusive: true)
    }

    public func writeAnchor(_ data: Data) throws {
        try ensureDirectory()
        try writeProtected(data, to: anchorURL, exclusive: true)
    }

    public func writeReview(_ data: Data, to url: URL) throws {
        let parent = url.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: parent, withIntermediateDirectories: true)
        try writeProtected(data, to: url)
    }

    private func writeProtected(_ data: Data, to url: URL, exclusive: Bool = false) throws {
        let flags = O_WRONLY | O_CREAT | O_NOFOLLOW | (exclusive ? O_EXCL : O_TRUNC)
        let fd = open(url.path, flags, 0o600)
        guard fd >= 0 else { throw posixFailure("cannot open \(url.lastPathComponent)") }
        defer { close(fd) }
        guard fchmod(fd, 0o600) == 0 else { throw posixFailure("cannot protect \(url.lastPathComponent)") }
        let written = data.withUnsafeBytes { raw -> Int in
            if raw.isEmpty { return 0 }
            var total = 0
            while total < raw.count {
                let amount = write(fd, raw.baseAddress!.advanced(by: total), raw.count - total)
                if amount <= 0 { return -1 }
                total += amount
            }
            return total
        }
        guard written == data.count, fsync(fd) == 0 else { throw posixFailure("cannot write \(url.lastPathComponent)") }
    }

    private func pathExists(_ url: URL) -> Bool {
        var info = stat()
        return lstat(url.path, &info) == 0 || errno != ENOENT
    }

    static func ensureProtectedDirectory(_ url: URL) throws {
        var info = stat()
        if lstat(url.path, &info) != 0 {
            guard errno == ENOENT else { throw posixFailure("cannot inspect signer directory") }
            let parent = url.deletingLastPathComponent()
            if parent.path != url.path {
                try FileManager.default.createDirectory(at: parent, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            }
            if mkdir(url.path, 0o700) != 0, errno != EEXIST {
                throw posixFailure("cannot create signer directory")
            }
            guard chmod(url.path, 0o700) == 0 else { throw posixFailure("cannot protect signer directory") }
            guard lstat(url.path, &info) == 0 else { throw posixFailure("cannot inspect signer directory") }
        }
        try verifyProtectedDirectory(url, info: info)
    }

    private static func verifyProtectedDirectory(_ url: URL, info: stat) throws {
        guard (info.st_mode & S_IFMT) == S_IFDIR else {
            throw SignerFailure(.failure, "signer directory is not a directory: \(url.path)")
        }
        guard info.st_uid == getuid() else {
            throw SignerFailure(.failure, "signer directory is not owned by the current uid: \(url.path)")
        }
        guard (info.st_mode & 0o7777) == 0o700 else {
            throw SignerFailure(.failure, "signer directory mode must be 0700: \(url.path)")
        }
    }
}

private func posixFailure(_ message: String) -> SignerFailure {
    SignerFailure(.failure, "\(message): \(String(cString: strerror(errno)))")
}

public struct AttestLogItem: Codable, Equatable {
    public let gateId: String
    public let decisionSequence: UInt64
    public let requestDigest: String
    public let decision: String

    public init(gateId: String, decisionSequence: UInt64, requestDigest: String, decision: String) {
        self.gateId = gateId
        self.decisionSequence = decisionSequence
        self.requestDigest = requestDigest
        self.decision = decision
    }
}

public struct RequestLogRecord: Codable, Equatable {
    public var timestamp: String
    public var callerPid: Int32
    public var callerExecutable: String
    public var consumer: String?
    public var gateId: String?
    public var decision: String?
    public var reasonHash16: String?
    public var requestDigest: String?
    public var correlationId: String?
    public var keyId: String?
    public var manifestDigest: String?
    public var count: Int?
    public var items: [AttestLogItem]?
    public var result: String

    public init(timestamp: String = "", callerPid: Int32 = 0, callerExecutable: String = "", consumer: String? = nil, gateId: String? = nil, decision: String? = nil, reasonHash16: String? = nil, requestDigest: String? = nil, correlationId: String? = nil, keyId: String? = nil, manifestDigest: String? = nil, count: Int? = nil, items: [AttestLogItem]? = nil, result: String = "") {
        self.timestamp = timestamp
        self.callerPid = callerPid
        self.callerExecutable = callerExecutable
        self.consumer = consumer
        self.gateId = gateId
        self.decision = decision
        self.reasonHash16 = reasonHash16
        self.requestDigest = requestDigest
        self.correlationId = correlationId
        self.keyId = keyId
        self.manifestDigest = manifestDigest
        self.count = count
        self.items = items
        self.result = result
    }
}

public final class RequestLogger {
    public static let promptLimit = 6
    public static let window: TimeInterval = 60

    private let url: URL
    private let now: () -> Date

    public init(url: URL, now: @escaping () -> Date = Date.init) {
        self.url = url
        self.now = now
    }

    public static func reasonHash16(_ reason: String) -> String {
        String(Canonical.keyId(publicKeyDER: Data(reason.utf8)).dropFirst("sha256:".count).prefix(16))
    }

    public func record(_ record: RequestLogRecord, result: String) throws {
        try withLockedLog { fd, _ in
            try append(record, result: result, fd: fd, at: now())
        }
    }

    public func performPrompt<T>(_ record: RequestLogRecord, operation: () throws -> T) throws -> T {
        try withLockedLog { fd, existing in
            let instant = now()
            if Self.promptCount(in: existing, since: instant.addingTimeInterval(-Self.window)) >= Self.promptLimit {
                try append(record, result: "ratelimited", fd: fd, at: instant)
                throw SignerFailure(.refused, "signing prompt rate limit exceeded")
            }
            do {
                let value = try operation()
                try append(record, result: "signed", fd: fd, at: instant)
                return value
            } catch {
                let result: String
                if let failure = error as? SignerFailure, failure.exit == .userCancelled {
                    result = "cancelled"
                } else {
                    result = "denied"
                }
                try append(record, result: result, fd: fd, at: instant)
                throw error
            }
        }
    }

    public static func promptCount(in data: Data, since: Date) -> Int {
        data.split(separator: 0x0a).reduce(into: 0) { count, line in
            guard let record = try? JSONDecoder().decode(RequestLogRecord.self, from: Data(line)),
                  record.result == "signed" || record.result == "cancelled" || record.result == "denied",
                  let date = TimeFormat.parse(record.timestamp), date >= since else { return }
            count += 1
        }
    }

    private func withLockedLog<T>(_ body: (Int32, Data) throws -> T) throws -> T {
        let parent = url.deletingLastPathComponent()
        try SignerStore.ensureProtectedDirectory(parent)
        let fd = open(url.path, O_RDWR | O_CREAT | O_APPEND | O_NOFOLLOW, 0o600)
        guard fd >= 0 else { throw SignerFailure(.refused, "cannot open request log") }
        defer { close(fd) }
        guard flock(fd, LOCK_EX) == 0 else { throw SignerFailure(.refused, "cannot lock request log") }
        defer { flock(fd, LOCK_UN) }
        _ = fchmod(fd, 0o600)
        guard lseek(fd, 0, SEEK_SET) >= 0 else { throw SignerFailure(.refused, "cannot read request log") }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while true {
            let amount = read(fd, &buffer, buffer.count)
            if amount < 0 { throw SignerFailure(.refused, "cannot read request log") }
            if amount == 0 { break }
            data.append(buffer, count: amount)
        }
        return try body(fd, data)
    }

    private func append(_ source: RequestLogRecord, result: String, fd: Int32, at date: Date) throws {
        var record = source
        record.timestamp = TimeFormat.string(date)
        if record.callerPid == 0 { record.callerPid = getppid() }
        if record.callerExecutable.isEmpty { record.callerExecutable = parentExecutable(pid: record.callerPid) }
        record.result = result
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        var bytes = try encoder.encode(record)
        bytes.append(0x0a)
        guard lseek(fd, 0, SEEK_END) >= 0 else { throw SignerFailure(.refused, "cannot append request log") }
        let written = bytes.withUnsafeBytes { raw in write(fd, raw.baseAddress, raw.count) }
        guard written == bytes.count, fsync(fd) == 0 else { throw SignerFailure(.refused, "cannot append request log") }
    }
}

private func parentExecutable(pid: Int32) -> String {
    var buffer = [CChar](repeating: 0, count: 4096)
    let count = proc_pidpath(pid, &buffer, UInt32(buffer.count))
    guard count > 0 else { return "unknown" }
    return String(cString: buffer)
}

public enum TimeFormat {
    private static let formatter: DateFormatter = {
        let value = DateFormatter()
        value.locale = Locale(identifier: "en_US_POSIX")
        value.calendar = Calendar(identifier: .gregorian)
        value.timeZone = TimeZone(secondsFromGMT: 0)
        value.dateFormat = "yyyy-MM-dd'T'HH:mm:ss'Z'"
        return value
    }()

    public static func string(_ date: Date) -> String { formatter.string(from: date) }
    public static func parse(_ value: String) -> Date? { formatter.date(from: value) }
}

public enum Randomness {
    public static func nonce() throws -> String {
        var bytes = [UInt8](repeating: 0, count: 16)
        guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else {
            throw SignerFailure(.failure, "cannot generate nonce")
        }
        return Data(bytes).hex
    }
}
