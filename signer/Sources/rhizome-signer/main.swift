import Darwin
import Foundation
import SignerCore

do {
    let arguments = Array(CommandLine.arguments.dropFirst())
    var stdin = Data()
    if arguments.first == "sign-stdin" {
        try CommandLineDriver.validateSignStdinArguments(arguments)
        stdin = try CommandLineDriver.readSignStdin(FileHandle.standardInput)
    }
    let output = try CommandLineDriver.run(arguments: arguments, stdin: stdin)
    FileHandle.standardOutput.write(output)
    exit(SignerExit.success.rawValue)
} catch let failure as SignerFailure {
    FileHandle.standardError.write(Data((failure.message + "\n").utf8))
    exit(failure.exit.rawValue)
} catch {
    FileHandle.standardError.write(Data("rhizome-signer failed\n".utf8))
    exit(SignerExit.failure.rawValue)
}
