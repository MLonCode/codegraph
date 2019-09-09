package git

import (
	"context"
	"crypto/md5"
	"encoding/hex"

	"github.com/cayleygraph/cayley/graph"
	"github.com/cayleygraph/cayley/quad"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/utils/merkletrie"
)

type ImportStats struct {
	Commits int
}

// AsQuads writes a git repository history as a set of quads.
// It returns the number of commits processed.
func AsQuads(w quad.Writer, gitpath string) (ImportStats, error) {
	repo, repoIRI, err := openGit(gitpath)
	if err != nil {
		return ImportStats{}, err
	}
	if err = writeGephiMetadata(w); err != nil {
		return ImportStats{}, err
	}
	imp := &importer{
		repo:    repo,
		repoIRI: repoIRI,
		w:       w,
		bw:      newSafeWriter(w),
	}
	return imp.Do()
}

type importer struct {
	repo    *git.Repository
	repoIRI quad.IRI
	w       quad.Writer
	bw      quad.BatchWriter
	stats   ImportStats
}

func (imp *importer) Do() (ImportStats, error) {
	err := imp.do()
	return imp.stats, err
}

func (imp *importer) do() error {
	if err := imp.w.WriteQuad(quad.Quad{
		Subject:   imp.repoIRI,
		Predicate: prdType,
		Object:    typeRepo,
	}); err != nil {
		return err
	}

	// import commits
	it, err := imp.repo.Log(&git.LogOptions{})
	if err != nil {
		return err
	}
	defer it.Close()

	err = it.ForEach(func(c *object.Commit) error {
		imp.stats.Commits++
		return imp.importCommit(c)
	})
	return err
}

// Import imports git repository into graph database
// Returns number of imported commits
func (g *Graph) Import(_ context.Context, gitpath string) (ImportStats, error) {
	w := graph.NewWriter(g.store)
	defer w.Close()

	return AsQuads(w, gitpath)
}

func openGit(gitpath string) (*git.Repository, quad.IRI, error) {
	repoIRI := quad.IRI(gitpath)

	// We instanciate a new repository targeting the given path (the .git folder)
	repo, err := git.PlainOpen(gitpath)
	if err != nil {
		return nil, "", err
	}
	if origin, err := repo.Remote("origin"); err == nil {
		if origin != nil && len(origin.Config().URLs) > 0 {
			repoIRI = quad.IRI(origin.Config().URLs[0])
		}
	}

	return repo, repoIRI, nil
}

func gitHashToIRI(h plumbing.Hash) quad.IRI {
	return quad.IRI("sha1:" + h.String())
}

func (imp *importer) importCommit(commit *object.Commit) error {
	commitIRI := gitHashToIRI(commit.Hash)

	if _, err := imp.bw.WriteQuads([]quad.Quad{
		{
			Subject:   imp.repoIRI,
			Predicate: prdCommit,
			Object:    commitIRI,
		},
		{
			Subject:   commitIRI,
			Predicate: prdType,
			Object:    typeCommit,
		},
		{
			Subject:   commitIRI,
			Predicate: prdMetadata,
			Object:    quad.String(commit.String()),
		},
		{
			Subject:   commitIRI,
			Predicate: prdMessage,
			Object:    quad.String(commit.Message),
		},
	}); err != nil {
		return err
	}
	if err := imp.importSignature(commitIRI, prdAuthor, commit.Author); err != nil {
		return err
	}
	if err := imp.importSignature(commitIRI, prdCommiter, commit.Committer); err != nil {
		return err
	}

	// dump parents
	for _, p := range commit.ParentHashes {
		if _, err := imp.bw.WriteQuads([]quad.Quad{
			{
				Subject:   commitIRI,
				Predicate: prdParent,
				Object:    gitHashToIRI(p),
			},
			{
				Subject:   gitHashToIRI(p),
				Predicate: prdChild,
				Object:    commitIRI,
			},
		}); err != nil {
			return err
		}
	}

	// import files
	it, err := commit.Files()
	if err != nil {
		return err
	}
	defer it.Close()
	if err := it.ForEach(func(f *object.File) error {
		return imp.importFile(commitIRI, f)
	}); err != nil {
		return err
	}

	// dump changes
	var (
		from *object.Tree
		to   *object.Tree
	)
	if commit.NumParents() > 0 {
		p, err := commit.Parent(0)
		if err != nil {
			return err
		}
		if from, err = p.Tree(); err != nil {
			return err
		}
	}
	if to, err = commit.Tree(); err != nil {
		return err
	}
	changes, err := object.DiffTree(from, to)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		if err = imp.importChange(commitIRI, ch); err != nil {
			return err
		}
	}
	return nil
}

func (imp *importer) importSignature(commit, pred quad.IRI, sig object.Signature) error {
	// auto-join authors on exact match
	h := md5.Sum([]byte(sig.Name + "\x00" + sig.Email))
	id := quad.BNode(hex.EncodeToString(h[:]))
	if _, err := imp.bw.WriteQuads([]quad.Quad{
		{
			Subject:   commit,
			Predicate: pred,
			Object:    id,
			Label:     quad.Time(sig.When),
		},
		{
			Subject:   id,
			Predicate: prdType,
			Object:    typeAuthor,
		},
		{
			Subject:   id,
			Predicate: prdName,
			Object:    quad.String(sig.Name),
		},
		{
			Subject:   id,
			Predicate: prdEmail,
			Object:    quad.IRI(sig.Email),
		},
	}); err != nil {
		return err
	}
	return nil
}

func (imp *importer) importFile(commitIRI quad.IRI, file *object.File) error {
	fileIRI := gitHashToIRI(file.Hash)

	if _, err := imp.bw.WriteQuads([]quad.Quad{
		{
			Subject:   commitIRI,
			Predicate: prdFile,
			Object:    fileIRI,
			Label:     quad.String(file.Name),
		},
		{
			Subject:   fileIRI,
			Predicate: prdType,
			Object:    typeFile,
		},
	}); err != nil {
		return err
	}
	return nil
}

func (imp *importer) importChange(commitIRI quad.IRI, change *object.Change) error {
	action, err := change.Action()
	if err != nil {
		return err
	}

	fromIRI := gitHashToIRI(change.From.TreeEntry.Hash)
	toIRI := gitHashToIRI(change.To.TreeEntry.Hash)

	switch action {
	case merkletrie.Delete:
		_, err = imp.bw.WriteQuads([]quad.Quad{{
			Subject:   fromIRI,
			Predicate: prdRemove,
			Object:    commitIRI,
			Label:     quad.String(change.From.Name),
		}})
	case merkletrie.Insert:
		_, err = imp.bw.WriteQuads([]quad.Quad{{
			Subject:   toIRI,
			Predicate: prdAdd,
			Object:    commitIRI,
			Label:     quad.String(change.To.Name),
		}})
	case merkletrie.Modify:
		_, err = imp.bw.WriteQuads([]quad.Quad{{
			Subject:   toIRI,
			Predicate: prdModify,
			Object:    commitIRI,
			Label:     quad.String(change.To.Name),
		}})
	}

	return err
}

func newSafeWriter(w quad.Writer) quad.BatchWriter {
	if bw, ok := w.(quad.BatchWriter); ok {
		return &batchWriter{bw: bw}
	}
	return &batchWriter{w: w}
}

type batchWriter struct {
	w  quad.Writer
	bw quad.BatchWriter
}

func (w *batchWriter) WriteQuads(buf []quad.Quad) (int, error) {
	if w.bw != nil {
		return w.bw.WriteQuads(buf)
	}
	for i, q := range buf {
		if err := w.w.WriteQuad(q); err != nil {
			return i, err
		}
	}
	return len(buf), nil
}
