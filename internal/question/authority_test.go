package question_test

import "rhizome/internal/trust"

import question "rhizome/internal/question"

func noAuthority() trust.Authority { return trust.Authority{} }

func mustQuestionID(title, body, recommendation string) string {
	id, err := question.IDFor(title, body, recommendation)
	if err != nil {
		panic(err)
	}
	return id
}

func mustQuestionIDForDigest(digest string) string {
	id, err := question.IDForDigest(digest)
	if err != nil {
		panic(err)
	}
	return id
}
