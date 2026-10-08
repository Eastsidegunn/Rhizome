package assembly

import (
	"rhizome/internal/question"
)

func mustQuestionID(title, body, recommendation string) string {
	id, err := question.IDFor(title, body, recommendation)
	if err != nil {
		panic(err)
	}
	return id
}
