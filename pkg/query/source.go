package query

import (
	"context"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/Jguer/aur"
	"github.com/Jguer/go-alpm/v2"
	"github.com/hashicorp/go-multierror"
	"github.com/leonelquinteros/gotext"

	"github.com/Jguer/yay/v12/pkg/db"
	"github.com/Jguer/yay/v12/pkg/intrange"
	"github.com/Jguer/yay/v12/pkg/settings/parser"
	"github.com/Jguer/yay/v12/pkg/stringset"
	"github.com/Jguer/yay/v12/pkg/text"

	"github.com/Jguer/aur/rpc"
)

type SearchVerbosity int

// Verbosity settings for search.
const (
	NumberMenu SearchVerbosity = iota
	Detailed
	Minimal
)

type SourceQueryBuilder struct {
	repoQuery
	aurQuery

	sortBy            string
	searchBy          string
	targetMode        parser.TargetMode
	useAURCache       bool
	bottomUp          bool
	singleLineResults bool

	aurClient rpc.ClientInterface
	aurCache  aur.QueryClient
	logger    *text.Logger
}

func NewSourceQueryBuilder(
	aurClient rpc.ClientInterface,
	aurCache aur.QueryClient,
	logger *text.Logger,
	sortBy string,
	targetMode parser.TargetMode,
	searchBy string,
	bottomUp,
	singleLineResults bool,
	useAURCache bool,
) *SourceQueryBuilder {
	return &SourceQueryBuilder{
		aurClient:         aurClient,
		aurCache:          aurCache,
		logger:            logger,
		repoQuery:         []alpm.IPackage{},
		aurQuery:          []aur.Pkg{},
		bottomUp:          bottomUp,
		sortBy:            sortBy,
		targetMode:        targetMode,
		searchBy:          searchBy,
		singleLineResults: singleLineResults,
		useAURCache:       useAURCache,
	}
}

func (s *SourceQueryBuilder) Execute(ctx context.Context,
	dbExecutor db.Executor,
	pkgS []string,
) {
	var aurErr error

	pkgS = RemoveInvalidTargets(pkgS, s.targetMode)

	if s.targetMode.AtLeastAUR() {
		s.aurQuery, aurErr = queryAUR(ctx, s.aurClient, s.aurCache, pkgS, s.searchBy, s.useAURCache)
		s.aurQuery = filterAURResults(pkgS, s.aurQuery)

		sort.Sort(aurSortable{aurQuery: s.aurQuery, sortBy: s.sortBy, bottomUp: s.bottomUp})
	}

	if s.targetMode.AtLeastRepo() {
		s.repoQuery = repoQuery(dbExecutor.SyncPackages(pkgS...))

		if s.bottomUp {
			s.Reverse()
		}
	}

	if aurErr != nil && len(s.repoQuery) != 0 {
		s.logger.Errorln(ErrAURSearch{inner: aurErr})
		s.logger.Warnln(gotext.Get("Showing repo packages only"))
	}
}

func (s *SourceQueryBuilder) Results(w io.Writer, dbExecutor db.Executor, verboseSearch SearchVerbosity) error {
	if s.aurQuery == nil || s.repoQuery == nil {
		return ErrNoQuery{}
	}

	if s.bottomUp {
		if s.targetMode.AtLeastAUR() {
			s.aurQuery.printSearch(w, len(s.repoQuery)+1, dbExecutor, verboseSearch, s.bottomUp, s.singleLineResults)
		}

		if s.targetMode.AtLeastRepo() {
			s.repoQuery.printSearch(w, dbExecutor, verboseSearch, s.bottomUp, s.singleLineResults)
		}
	} else {
		if s.targetMode.AtLeastRepo() {
			s.repoQuery.printSearch(w, dbExecutor, verboseSearch, s.bottomUp, s.singleLineResults)
		}

		if s.targetMode.AtLeastAUR() {
			s.aurQuery.printSearch(w, len(s.repoQuery)+1, dbExecutor, verboseSearch, s.bottomUp, s.singleLineResults)
		}
	}

	return nil
}

func (s *SourceQueryBuilder) Len() int {
	return len(s.repoQuery) + len(s.aurQuery)
}

func (s *SourceQueryBuilder) GetTargets(include, exclude intrange.IntRanges,
	otherExclude stringset.StringSet,
) ([]string, error) {
	isInclude := len(exclude) == 0 && len(otherExclude) == 0

	var targets []string

	for i, pkg := range s.repoQuery {
		var target int

		if s.bottomUp {
			target = len(s.repoQuery) - i
		} else {
			target = i + 1
		}

		if (isInclude && include.Get(target)) || (!isInclude && !exclude.Get(target)) {
			targets = append(targets, pkg.DB().Name()+"/"+pkg.Name())
		}
	}

	for i := range s.aurQuery {
		var target int

		if s.bottomUp {
			target = len(s.aurQuery) - i + len(s.repoQuery)
		} else {
			target = i + 1 + len(s.repoQuery)
		}

		if (isInclude && include.Get(target)) || (!isInclude && !exclude.Get(target)) {
			targets = append(targets, "aur/"+s.aurQuery[i].Name)
		}
	}

	return targets, nil
}

// filter AUR results to remove strings that don't contain all of the search terms.
func filterAURResults(pkgS []string, results []aur.Pkg) []aur.Pkg {
	aurPkgs := make([]aur.Pkg, 0, len(results))
	matchesSearchTerms := func(pkg *aur.Pkg, terms []string) bool {
		for _, pkgN := range terms {
			if strings.IndexFunc(pkgN, unicode.IsSymbol) != -1 {
				return true
			}
			name := strings.ToLower(pkg.Name)
			desc := strings.ToLower(pkg.Description)
			targ := strings.ToLower(pkgN)

			if !(strings.Contains(name, targ) || strings.Contains(desc, targ)) {
				return false
			}
		}

		return true
	}

	for i := range results {
		if matchesSearchTerms(&results[i], pkgS) {
			aurPkgs = append(aurPkgs, results[i])
		}
	}

	return aurPkgs
}

// queryAUR searches AUR and narrows based on subarguments.
func queryAUR(ctx context.Context,
	rpcClient rpc.ClientInterface, aurClient aur.QueryClient,
	pkgS []string, searchBy string, newEngine bool,
) ([]aur.Pkg, error) {
	var (
		err error
		by  = getSearchBy(searchBy)
	)

	queryClient := aurClient
	if !newEngine {
		queryClient = rpcClient
	}

	for _, word := range pkgS {
		r, errM := queryClient.Get(ctx, &aur.Query{
			Needles:  []string{word},
			By:       by,
			Contains: true,
		})
		if errM == nil {
			return r, nil
		}

		err = multierror.Append(err, errM)
	}

	return nil, err
}
