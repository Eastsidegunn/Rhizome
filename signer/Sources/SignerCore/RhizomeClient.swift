import Darwin
import Foundation

public enum RhizomeURLPolicy {
    public static let defaultURL = "http://127.0.0.1:8790"

    public static func validate(_ value: String) throws -> URL {
        guard let components = URLComponents(string: value),
              components.scheme == "http",
              components.user == nil,
              components.password == nil,
              components.query == nil,
              components.fragment == nil,
              let host = components.host,
              components.path.isEmpty || components.path == "/",
              let url = components.url,
              isLoopbackIP(unbracketed(host)) else {
            throw SignerFailure(.refused, "Rhizome URL must be an HTTP loopback IP")
        }
        return url
    }

    private static func isLoopbackIP(_ host: String) -> Bool {
        var v4 = in_addr()
        if inet_pton(AF_INET, host, &v4) == 1 {
            return (UInt32(bigEndian: v4.s_addr) >> 24) == 127
        }
        var v6 = in6_addr()
        if inet_pton(AF_INET6, host, &v6) == 1 {
            return withUnsafeBytes(of: &v6) { bytes in
                bytes.dropLast().allSatisfy { $0 == 0 } && bytes.last == 1
            }
        }
        return false
    }

    private static func unbracketed(_ host: String) -> String {
        guard host.first == "[", host.last == "]" else { return host }
        return String(host.dropFirst().dropLast())
    }
}

public final class NoRedirectDelegate: NSObject, URLSessionTaskDelegate {
    public static let followsRedirects = false

    public func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
        completionHandler(nil)
    }
}

public enum HTTPPolicy {
    public static func configuration() -> URLSessionConfiguration {
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 10
        config.timeoutIntervalForResource = 10
        config.connectionProxyDictionary = [:]
        config.httpShouldUsePipelining = false
        return config
    }
}

public protocol RhizomeClientProtocol {
    func signingInput(gateId: String) throws -> SigningInput
    func unverifiedInputs() throws -> SigningList
    func submit(intent: Data) throws -> Data
}

public final class RhizomeClient: RhizomeClientProtocol {
    private let baseURL: URL
    private let session: URLSession

    public init(baseURL: URL) {
        self.baseURL = baseURL
        self.session = URLSession(configuration: HTTPPolicy.configuration(), delegate: NoRedirectDelegate(), delegateQueue: nil)
    }

    public func signingInput(gateId: String) throws -> SigningInput {
        let data = try get(query: URLQueryItem(name: "gate", value: gateId))
        return try StrictJSON.signingInput(data)
    }

    public func unverifiedInputs() throws -> SigningList {
        let data = try get(query: URLQueryItem(name: "unverified", value: "1"))
        return try StrictJSON.signingList(data)
    }

    public func submit(intent: Data) throws -> Data {
        let url = baseURL.appendingPathComponent("v1/intent")
        var request = URLRequest(url: url, timeoutInterval: 10)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = intent
        return try perform(request)
    }

    private func get(query: URLQueryItem) throws -> Data {
        var components = URLComponents(url: baseURL.appendingPathComponent("v1/trust/signing"), resolvingAgainstBaseURL: false)!
        components.queryItems = [query]
        var request = URLRequest(url: components.url!, timeoutInterval: 10)
        request.httpMethod = "GET"
        request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        return try perform(request)
    }

    private func perform(_ request: URLRequest) throws -> Data {
        let semaphore = DispatchSemaphore(value: 0)
        var result: Result<Data, Error>!
        session.dataTask(with: request) { data, response, error in
            defer { semaphore.signal() }
            if let error {
                result = .failure(error)
                return
            }
            guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data else {
                result = .failure(SignerFailure(.rhizomeReadFailed, "Rhizome request failed"))
                return
            }
            result = .success(data)
        }.resume()
        if semaphore.wait(timeout: .now() + 11) == .timedOut {
            throw SignerFailure(.rhizomeReadFailed, "Rhizome request timed out")
        }
        do { return try result.get() }
        catch let failure as SignerFailure { throw failure }
        catch { throw SignerFailure(.rhizomeReadFailed, "Rhizome request failed") }
    }
}

public enum StrictJSON {
    public static func signingInput(_ data: Data) throws -> SigningInput {
        let object = try dictionary(data)
        let common: Set<String> = ["journalId", "consumer", "gateId", "title", "requestDigest", "state", "verificationStatus"]
        let decisionKeys: Set<String> = ["decisionSequence", "decision", "reason"]
        let actual = Set(object.keys)
        let anchoredCommon = common
        let anchorlessCommon = common.subtracting(["journalId"])
        guard actual == anchoredCommon || actual == anchorlessCommon || actual == anchoredCommon.union(decisionKeys) || actual == anchorlessCommon.union(decisionKeys) else {
            throw SignerFailure(.rhizomeReadFailed, "invalid signing response schema")
        }
        let hasDecision = actual.isSuperset(of: decisionKeys)
        guard let consumer = object["consumer"] as? String, ["question", "approval"].contains(consumer),
              let gateId = object["gateId"] as? String,
              let title = object["title"] as? String,
              let requestDigest = object["requestDigest"] as? String,
              let state = object["state"] as? String,
              let status = object["verificationStatus"] as? String,
              ["none", "claimed", "legacy-asserted", "verified", "attested"].contains(status) else {
            throw SignerFailure(.rhizomeReadFailed, "invalid signing response")
        }
        let sequence = hasDecision ? uint64(object["decisionSequence"]) : nil
        let decision = hasDecision ? object["decision"] as? String : nil
        let reason = hasDecision ? object["reason"] as? String : nil
        if hasDecision && (sequence == nil || decision == nil || reason == nil) {
            throw SignerFailure(.rhizomeReadFailed, "invalid signing decision response")
        }
        return SigningInput(journalId: object["journalId"] as? String, consumer: consumer, gateId: gateId, title: title, requestDigest: requestDigest, state: state, verificationStatus: status, decisionSequence: sequence, decision: decision, reason: reason)
    }

    public static func signingList(_ data: Data) throws -> SigningList {
        let object = try dictionary(data)
        let actual = Set(object.keys)
        guard actual == ["journalId", "items"] || actual == ["items"], let rawItems = object["items"] as? [[String: Any]] else {
            throw SignerFailure(.rhizomeReadFailed, "invalid signing list schema")
        }
        let expected: Set<String> = ["consumer", "gateId", "decisionSequence", "requestDigest", "decision", "reason", "title"]
        var items: [AttestItem] = []
        for item in rawItems {
            guard Set(item.keys) == expected,
                  let consumer = item["consumer"] as? String,
                  let gateId = item["gateId"] as? String,
                  let sequence = uint64(item["decisionSequence"]),
                  let digest = item["requestDigest"] as? String,
                  let decision = item["decision"] as? String,
                  let reason = item["reason"] as? String,
                  let title = item["title"] as? String else {
                throw SignerFailure(.rhizomeReadFailed, "invalid signing list item")
            }
            items.append(AttestItem(consumer: consumer, gateId: gateId, decisionSequence: sequence, digest: digest, decision: decision, reason: reason, title: title))
        }
        return SigningList(journalId: object["journalId"] as? String, items: items)
    }

    public static func oneObject(_ data: Data, exactKeys: Set<String>) throws -> [String: Any] {
        let object = try dictionary(data)
        guard Set(object.keys) == exactKeys else { throw SignerFailure(.refused, "invalid input schema") }
        return object
    }

    private static func dictionary(_ data: Data) throws -> [String: Any] {
        do {
            guard let value = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                throw SignerFailure(.rhizomeReadFailed, "expected one JSON object")
            }
            return value
        } catch let failure as SignerFailure {
            throw failure
        } catch {
            throw SignerFailure(.rhizomeReadFailed, "invalid JSON")
        }
    }

    private static func uint64(_ value: Any?) -> UInt64? {
        guard let number = value as? NSNumber else { return nil }
        let decimal = number.decimalValue
        guard decimal >= 0, decimal == Decimal(number.uint64Value) else { return nil }
        return number.uint64Value
    }
}
