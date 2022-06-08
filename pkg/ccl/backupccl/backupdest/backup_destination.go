// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package backupdest

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/ccl/backupccl/backupbase"
	"github.com/cockroachdb/cockroach/pkg/ccl/backupccl/backuputils"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/ioctx"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

const (
	// LocalityURLParam is the parameter name used when specifying a locality tag
	// in a locality aware backup/restore.
	LocalityURLParam = "COCKROACH_LOCALITY"
	// DefaultLocalityValue is the default locality tag used in a locality aware
	// backup/restore when an explicit COCKROACH_LOCALITY is not specified.
	DefaultLocalityValue = "default"
)

// On some cloud storage platforms (i.e. GS, S3), backups in a base bucket may
// omit a leading slash. However, backups in a subdirectory of a base bucket
// will contain one.
var backupPathRE = regexp.MustCompile("^/?[^\\/]+/[^\\/]+/[^\\/]+/" + backupbase.BackupManifestName + "$")

// featureFullBackupUserSubdir, when true, will create a full backup at a user
// specified subdirectory if no backup already exists at that subdirectory. As
// of 22.1, this feature is default disabled, and will be totally disabled by 22.2.
var featureFullBackupUserSubdir = settings.RegisterBoolSetting(
	settings.TenantWritable,
	"bulkio.backup.deprecated_full_backup_with_subdir.enabled",
	"when true, a backup command with a user specified subdirectory will create a full backup at"+
		" the subdirectory if no backup already exists at that subdirectory.",
	false,
).WithPublic()

// TODO(adityamaru): Move this to the soon to be `backupinfo` package.
func containsManifest(ctx context.Context, exportStore cloud.ExternalStorage) (bool, error) {
	r, err := exportStore.ReadFile(ctx, backupbase.BackupManifestName)
	if err != nil {
		if errors.Is(err, cloud.ErrFileDoesNotExist) {
			return false, nil
		}
		return false, err
	}
	r.Close(ctx)
	return true, nil
}

// ResolveDest resolves the true destination of a backup. The backup command
// provided by the user may point to a backup collection, or a backup location
// which auto-appends incremental backups to it. This method checks for these
// cases and finds the actual directory where we'll write this new backup.
//
// In addition, in this case that this backup is an incremental backup (either
// explicitly, or due to the auto-append feature), it will resolve the
// encryption options based on the base backup, as well as find all previous
// backup manifests in the backup chain.
func ResolveDest(
	ctx context.Context,
	user username.SQLUsername,
	dest jobspb.BackupDetails_Destination,
	endTime hlc.Timestamp,
	incrementalFrom []string,
	execCfg *sql.ExecutorConfig,
) (
	collectionURI string,
	plannedBackupDefaultURI string, /* the full path for the planned backup */
	/* chosenSuffix is the automatically chosen suffix within the collection path
	   if we're backing up INTO a collection. */
	chosenSuffix string,
	urisByLocalityKV map[string]string,
	prevBackupURIs []string, /* list of full paths for previous backups in the chain */
	err error,
) {
	makeCloudStorage := execCfg.DistSQLSrv.ExternalStorageFromURI

	defaultURI, _, err := GetURIsByLocalityKV(dest.To, "")
	if err != nil {
		return "", "", "", nil, nil, err
	}

	chosenSuffix = dest.Subdir

	if chosenSuffix != "" {
		// The legacy backup syntax, BACKUP TO, leaves the dest.Subdir and collection parameters empty.
		collectionURI = defaultURI

		if chosenSuffix == backupbase.LatestFileName {
			latest, err := ReadLatestFile(ctx, defaultURI, makeCloudStorage, user)
			if err != nil {
				return "", "", "", nil, nil, err
			}
			chosenSuffix = latest
		}
	}

	plannedBackupDefaultURI, urisByLocalityKV, err = GetURIsByLocalityKV(dest.To, chosenSuffix)
	if err != nil {
		return "", "", "", nil, nil, err
	}

	// At this point, the plannedBackupDefaultURI is the full path for the backup. For BACKUP
	// INTO, this path includes the chosenSuffix. Once this function returns, the
	// plannedBackupDefaultURI will be the full path for this backup in planning.
	if len(incrementalFrom) != 0 {
		// Legacy backup with deprecated BACKUP TO-syntax.
		prevBackupURIs = incrementalFrom
		return collectionURI, plannedBackupDefaultURI, chosenSuffix, urisByLocalityKV, prevBackupURIs, nil
	}

	defaultStore, err := makeCloudStorage(ctx, plannedBackupDefaultURI, user)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	defer defaultStore.Close()
	exists, err := containsManifest(ctx, defaultStore)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	if exists && !dest.Exists && chosenSuffix != "" && execCfg.Settings.Version.IsActive(ctx,
		clusterversion.Start22_1) {
		// We disallow a user from writing a full backup to a path in a collection containing an
		// existing backup iff we're 99.9% confident this backup was planned on a 22.1 node.
		return "",
			"",
			"",
			nil,
			nil,
			errors.Newf("A full backup already exists in %s. "+
				"Consider running an incremental backup to this full backup via `BACKUP INTO '%s' IN '%s'`",
				plannedBackupDefaultURI, chosenSuffix, dest.To[0])

	} else if !exists {
		if dest.Exists {
			// Implies the user passed a subdirectory in their backup command, either
			// explicitly or using LATEST; however, we could not find an existing
			// backup in that subdirectory.
			// - Pre 22.1: this was fine. we created a full backup in their specified subdirectory.
			// - 22.1: throw an error: full backups with an explicit subdirectory are deprecated.
			// User can use old behavior by switching the 'bulkio.backup.full_backup_with_subdir.
			// enabled' to true.
			// - 22.2+: the backup will fail unconditionally.
			// TODO (msbutler): throw error in 22.2
			if !featureFullBackupUserSubdir.Get(execCfg.SV()) {
				return "", "", "", nil, nil,
					errors.Errorf("A full backup cannot be written to %q, a user defined subdirectory. "+
						"To take a full backup, remove the subdirectory from the backup command "+
						"(i.e. run 'BACKUP ... INTO <collectionURI>'). "+
						"Or, to take a full backup at a specific subdirectory, "+
						"enable the deprecated syntax by switching the %q cluster setting to true; "+
						"however, note this deprecated syntax will not be available in a future release.",
						chosenSuffix, featureFullBackupUserSubdir.Key())
			}
		}
		// There's no full backup in the resolved subdirectory; therefore, we're conducting a full backup.
		return collectionURI, plannedBackupDefaultURI, chosenSuffix, urisByLocalityKV, prevBackupURIs, nil
	}

	// The defaultStore contains a full backup; consequently, we're conducting an incremental backup.
	fullyResolvedIncrementalsLocation, err := ResolveIncrementalsBackupLocation(
		ctx,
		user,
		execCfg,
		dest.IncrementalStorage,
		dest.To,
		chosenSuffix)
	if err != nil {
		return "", "", "", nil, nil, err
	}

	priorsDefaultURI, _, err := GetURIsByLocalityKV(fullyResolvedIncrementalsLocation, "")
	if err != nil {
		return "", "", "", nil, nil, err
	}
	incrementalStore, err := makeCloudStorage(ctx, priorsDefaultURI, user)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	defer incrementalStore.Close()

	priors, err := FindPriorBackups(ctx, incrementalStore, backupbase.OmitManifest)
	if err != nil {
		return "", "", "", nil, nil, errors.Wrap(err, "adjusting backup destination to append new layer to existing backup")
	}

	for _, prior := range priors {
		priorURI, err := url.Parse(priorsDefaultURI)
		if err != nil {
			return "", "", "", nil, nil, errors.Wrapf(err, "parsing default backup location %s",
				priorsDefaultURI)
		}
		priorURI.Path = backuputils.JoinURLPath(priorURI.Path, prior)
		prevBackupURIs = append(prevBackupURIs, priorURI.String())
	}
	prevBackupURIs = append([]string{plannedBackupDefaultURI}, prevBackupURIs...)

	// Within the chosenSuffix dir, differentiate incremental backups with partName.
	partName := endTime.GoTime().Format(backupbase.DateBasedIncFolderName)
	defaultIncrementalsURI, urisByLocalityKV, err := GetURIsByLocalityKV(fullyResolvedIncrementalsLocation, partName)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	return collectionURI, defaultIncrementalsURI, chosenSuffix, urisByLocalityKV, prevBackupURIs, nil
}

// ReadLatestFile reads the LATEST file from collectionURI and returns the path
// stored in the file.
func ReadLatestFile(
	ctx context.Context,
	collectionURI string,
	makeCloudStorage cloud.ExternalStorageFromURIFactory,
	user username.SQLUsername,
) (string, error) {
	collection, err := makeCloudStorage(ctx, collectionURI, user)
	if err != nil {
		return "", err
	}
	defer collection.Close()

	latestFile, err := FindLatestFile(ctx, collection)

	if err != nil {
		if errors.Is(err, cloud.ErrFileDoesNotExist) {
			return "", pgerror.Wrapf(err, pgcode.UndefinedFile, "path does not contain a completed latest backup")
		}
		return "", pgerror.WithCandidateCode(err, pgcode.Io)
	}
	latest, err := ioctx.ReadAll(ctx, latestFile)
	if err != nil {
		return "", err
	}
	if len(latest) == 0 {
		return "", errors.Errorf("malformed LATEST file")
	}
	return string(latest), nil
}

// FindLatestFile returns a ioctx.ReaderCloserCtx of the most recent LATEST
// file. First it tries reading from the latest directory. If
// the backup is from an older version, it may not exist there yet so
// it tries reading in the base directory if the first attempt fails.
func FindLatestFile(
	ctx context.Context, exportStore cloud.ExternalStorage,
) (ioctx.ReadCloserCtx, error) {
	var latestFile string
	var latestFileFound bool
	// First try reading from the metadata/latest directory. If the backup
	// is from an older version, it may not exist there yet so try reading
	// in the base directory if the first attempt fails.

	// We name files such that the most recent latest file will always
	// be at the top, so just grab the first filename.
	err := exportStore.List(ctx, backupbase.LatestHistoryDirectory, "", func(p string) error {
		p = strings.TrimPrefix(p, "/")
		latestFile = p
		latestFileFound = true
		// We only want the first latest file so return an error that it is
		// done listing.
		return cloud.ErrListingDone
	})
	// If the list failed because the storage used does not support listing,
	// such as http, we can try reading the non-timestamped backup latest
	// file directly. This can still fail if it is a mixed cluster and the
	// latest file was written in the base directory.
	if errors.Is(err, cloud.ErrListingUnsupported) {
		r, err := exportStore.ReadFile(ctx, backupbase.LatestHistoryDirectory+"/"+backupbase.LatestFileName)
		if err == nil {
			return r, nil
		}
	} else if err != nil && !errors.Is(err, cloud.ErrListingDone) {
		return nil, err
	}

	if latestFileFound {
		return exportStore.ReadFile(ctx, backupbase.LatestHistoryDirectory+"/"+latestFile)
	}

	// The latest file couldn't be found in the latest directory,
	// try the base directory instead.
	r, err := exportStore.ReadFile(ctx, backupbase.LatestFileName)
	if err != nil {
		return nil, errors.Wrap(err, "LATEST file could not be read in base or metadata directory")
	}
	return r, nil
}

// WriteNewLatestFile writes a new LATEST file to both the base directory
// and latest-history directory, depending on cluster version.
func WriteNewLatestFile(
	ctx context.Context, settings *cluster.Settings, exportStore cloud.ExternalStorage, suffix string,
) error {
	// If the cluster is still running on a mixed version, we want to write
	// to the base directory instead of the metadata/latest directory. That
	// way an old node can still find the LATEST file.
	if !settings.Version.IsActive(ctx, clusterversion.BackupDoesNotOverwriteLatestAndCheckpoint) {
		return cloud.WriteFile(ctx, exportStore, backupbase.LatestFileName, strings.NewReader(suffix))
	}

	// HTTP storage does not support listing and so we cannot rely on the
	// above-mentioned List method to return us the most recent latest file.
	// Instead, we disregard write once semantics and always read and write
	// a non-timestamped latest file for HTTP.
	if exportStore.Conf().Provider == roachpb.ExternalStorageProvider_http {
		return cloud.WriteFile(ctx, exportStore, backupbase.LatestFileName, strings.NewReader(suffix))
	}

	// We timestamp the latest files in order to enforce write once backups.
	// When the job goes to read these timestamped files, it will List
	// the latest files and pick the file whose name is lexicographically
	// sorted to the top. This will be the last latest file we write. It
	// Takes the one's complement of the timestamp so that files are sorted
	// lexicographically such that the most recent is always the top.
	return cloud.WriteFile(ctx, exportStore, newTimestampedLatestFileName(), strings.NewReader(suffix))
}

// newTimestampedLatestFileName returns a string of a new latest filename
// with a suffixed version. It returns it in the format of LATEST-<version>
// where version is a hex encoded one's complement of the timestamp.
// This means that as long as the supplied timestamp is correct, the filenames
// will adhere to a lexicographical/utf-8 ordering such that the most
// recent file is at the top.
func newTimestampedLatestFileName() string {
	var buffer []byte
	buffer = encoding.EncodeStringDescending(buffer, timeutil.Now().String())
	return fmt.Sprintf("%s/%s-%s", backupbase.LatestHistoryDirectory, backupbase.LatestFileName, hex.EncodeToString(buffer))
}

// CheckForLatestFileInCollection checks whether the directory pointed by store contains the
// latestFileName pointer directory.
func CheckForLatestFileInCollection(
	ctx context.Context, store cloud.ExternalStorage,
) (bool, error) {
	r, err := FindLatestFile(ctx, store)
	if err != nil {
		if !errors.Is(err, cloud.ErrFileDoesNotExist) {
			return false, pgerror.WithCandidateCode(err, pgcode.Io)
		}

		r, err = store.ReadFile(ctx, backupbase.LatestFileName)
	}
	if err != nil {
		if errors.Is(err, cloud.ErrFileDoesNotExist) {
			return false, nil
		}
		return false, pgerror.WithCandidateCode(err, pgcode.Io)
	}
	r.Close(ctx)
	return true, nil
}

func getLocalityAndBaseURI(uri, appendPath string) (string, string, error) {
	parsedURI, err := url.Parse(uri)
	if err != nil {
		return "", "", err
	}
	q := parsedURI.Query()
	localityKV := q.Get(LocalityURLParam)
	// Remove the backup locality parameter.
	q.Del(LocalityURLParam)
	parsedURI.RawQuery = q.Encode()

	parsedURI.Path = backuputils.JoinURLPath(parsedURI.Path, appendPath)

	baseURI := parsedURI.String()
	return localityKV, baseURI, nil
}

// GetURIsByLocalityKV takes a slice of URIs for a single (possibly partitioned)
// backup, and returns the default backup destination URI and a map of all other
// URIs by locality KV, appending appendPath to the path component of both the
// default URI and all the locality URIs. The URIs in the result do not include
// the COCKROACH_LOCALITY parameter.
func GetURIsByLocalityKV(
	to []string, appendPath string,
) (defaultURI string, urisByLocalityKV map[string]string, err error) {
	urisByLocalityKV = make(map[string]string)
	if len(to) == 1 {
		localityKV, baseURI, err := getLocalityAndBaseURI(to[0], appendPath)
		if err != nil {
			return "", nil, err
		}
		if localityKV != "" && localityKV != DefaultLocalityValue {
			return "", nil, errors.Errorf("%s %s is invalid for a single BACKUP location",
				LocalityURLParam, localityKV)
		}
		return baseURI, urisByLocalityKV, nil
	}

	for _, uri := range to {
		localityKV, baseURI, err := getLocalityAndBaseURI(uri, appendPath)
		if err != nil {
			return "", nil, err
		}
		if localityKV == "" {
			return "", nil, errors.Errorf(
				"multiple URLs are provided for partitioned BACKUP, but %s is not specified",
				LocalityURLParam,
			)
		}
		if localityKV == DefaultLocalityValue {
			if defaultURI != "" {
				return "", nil, errors.Errorf("multiple default URLs provided for partition backup")
			}
			defaultURI = baseURI
		} else {
			kv := roachpb.Tier{}
			if err := kv.FromString(localityKV); err != nil {
				return "", nil, errors.Wrap(err, "failed to parse backup locality")
			}
			if _, ok := urisByLocalityKV[localityKV]; ok {
				return "", nil, errors.Errorf("duplicate URIs for locality %s", localityKV)
			}
			urisByLocalityKV[localityKV] = baseURI
		}
	}
	if defaultURI == "" {
		return "", nil, errors.Errorf("no default URL provided for partitioned backup")
	}
	return defaultURI, urisByLocalityKV, nil
}

// ListFullBackupsInCollection lists full backup paths in the collection
// of an export store
func ListFullBackupsInCollection(
	ctx context.Context, store cloud.ExternalStorage,
) ([]string, error) {
	var backupPaths []string
	if err := store.List(ctx, "", listingDelimDataSlash, func(f string) error {
		if backupPathRE.MatchString(f) {
			backupPaths = append(backupPaths, f)
		}
		return nil
	}); err != nil {
		// Can't happen, just required to handle the error for lint.
		return nil, err
	}
	for i, backupPath := range backupPaths {
		backupPaths[i] = strings.TrimSuffix(backupPath, "/"+backupbase.BackupManifestName)
	}
	return backupPaths, nil
}