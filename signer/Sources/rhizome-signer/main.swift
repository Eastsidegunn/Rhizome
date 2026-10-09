import Darwin
import Foundation
import SignerCore

do {
    let output = try CommandLineDriver.run(arguments: Array(CommandLine.arguments.dropFirst()), stdin: FileHandle.standardInput.readDataToEndOfFile())
    FileHandle.standardOutput.write(output)
    exit(SignerExit.success.rawValue)
} catch let failure as SignerFailure {
    FileHandle.standardError.write(Data((failure.message + "\n").utf8))
    exit(failure.exit.rawValue)
} catch {
    FileHandle.standardError.write(Data("rhizome-signer failed\n".utf8))
    exit(SignerExit.failure.rawValue)
}
