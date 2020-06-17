package repoutil

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/Donders-Institute/tg-toolset-golang/pkg/logger"
	"github.com/Donders-Institute/tg-toolset-golang/project/pkg/pdb"
	"github.com/Donders-Institute/tg-toolset-golang/project/pkg/repo"
	"github.com/spf13/cobra"

	bolt "go.etcd.io/bbolt"
)

var exportNthreads int
var exportOUs string
var exportCollPat string
var viewerDbPath string

// CollExport maintains a data structure for operating on
// a repository collection to be exported.
type CollExport struct {
	Path        string   `json:"path"`
	OU          string   `json:"ou"`
	ViewersRepo []string `json:"viewersRepo"`
}

// Umap is the data structure maps repo username to local username.
// The link is the email.
type Umap struct {
	Email    string `json:"email"`
	UIDLocal string `json:"uidLocal"`
	UIDRepo  string `json:"uidRepo"`
}

func init() {

	cwd, _ := os.Getwd()

	exportCmd.Flags().IntVarP(&exportNthreads, "nthreads", "n", 4, "`number` of concurrent worker threads.")
	exportCmd.Flags().StringVarP(
		&exportOUs,
		"ou", "", "dccn",
		"comma-separated repository OUs from which the collections are exported.",
	)
	exportCmd.Flags().StringVarP(
		&exportCollPat,
		"pat", "", "*:v*",
		"name pattern of collections to be exported.",
	)
	exportCmd.Flags().StringVarP(
		&viewerDbPath,
		"db", "", filepath.Join(cwd, ".export.db"),
		"path of the local viewer db.",
	)
	rootCmd.AddCommand(exportCmd)
}

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Utility for exporting repository collections to local users",
	Long:  ``,
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {

		// load pdb
		pdb := loadPdb()

		// load viewerdb
		vdb := store{
			path: viewerDbPath,
		}

		// connect to db
		if err := vdb.connect(); err != nil {
			return err
		}
		defer vdb.disconnect()

		// always run vdb.init() to make sure buckets are created.
		if err := vdb.init(); err != nil {
			return err
		}

		// load data map of the currently exported collections.
		cms := make(map[string]interface{})
		if err := vdb.getAll("cmap", cms); err != nil {
			return fmt.Errorf("fail to load existing collmap: %s", err)
		}
		log.Debugf("currently exported: %+v", cms)

		// retrieve current collection roles of collections to be exported concurrently.
		chanCollExport := make(chan CollExport, 2*exportNthreads)
		var wg1 sync.WaitGroup
		for w := 0; w < exportNthreads; w++ {
			wg1.Add(1)
			go func() {
				defer wg1.Done()
				for coll := range chanCollExport {
					n := filepath.Base(coll.Path)

					// create symlink in RepoExportPath
					linkExport := filepath.Join(RepoExportPath, coll.OU, n)
					if _, err := os.Stat(linkExport); os.IsNotExist(err) {
						if err := os.Symlink(coll.Path, linkExport); err != nil {
							// log error and move onto the next collection.
							log.Errorf("[%s] %s", n, err)
							continue
						}
					}

					// get users with a role in the collection
					collHead := filepath.Join(
						RepoNamespace,
						coll.OU,
						n[0:strings.Index(n, ":v")],
					)
					cmd := fmt.Sprintf(`iquest --no-page "%%s" "select META_COLL_ATTR_VALUE where COLL_NAME = '%s' and META_COLL_ATTR_NAME in ('manager','contributor','viewerByDUA','viewerByManager')" | sort`, collHead)
					cout := make(chan string)
					go repo.IcommandChanOut(cmd, &cout, true)

					// urep is a map with repo users who have repo access to the the collection.
					urep := make(map[string]bool)
					for u := range cout {
						urep[u] = true
					}

					// uloc is a list of repo users who have local access to the collection.
					uloc := make(map[string]bool)
					if cexp, ok := cms[n]; ok {
						for _, u := range cexp.(*CollExport).ViewersRepo {
							uloc[u] = true
						}
					}

					// uadd is a list of users to be added for local access.
					uadd := []Umap{}
					acl := ""
					for u := range urep {

						// user already has local access, skip.
						if _, ok := uloc[u]; ok {
							continue
						}

						// resolve umap of the user.
						um, err := findUmap(pdb, vdb, u)
						if err != nil {
							log.Warnf("[%s] cannot map repo user to local user: %s", n, u)
							continue
						}
						log.Debugf("[%s] umap: %+v", um)

						uadd = append(uadd, um)
						acl = fmt.Sprintf("%s:r-x,%s", um.UIDLocal, acl)
					}

					log.Debugf("[%s] users to be added: %+v", n, uadd)

					// run the `setfacl -m` command.
					if acl != "" {
						cmd = fmt.Sprintf("setfacl -m %s %s", acl, coll.Path)
						if err := repo.IcommandFileOut(cmd, ""); err != nil {
							log.Errorf("[%s] fail setting acl: %s", n, err)
						} else {
							// update uloc with users being added for accecc.
							for _, u := range uadd {
								uloc[u.UIDRepo] = true
							}
						}
					}

					// udel is a list of users to be removed from local access.
					udel := []Umap{}
					acl = ""
					for u := range uloc {

						// user has repo access, no need to remove the user from local access.
						if _, ok := urep[u]; ok {
							continue
						}

						// resolve umap of the user.
						um, err := findUmap(pdb, vdb, u)
						if err != nil {
							log.Warnf("[%s] cannot map repo user to local user: %s", n, u)
							continue
						}
						log.Debugf("[%s] umap: %+v", um)

						udel = append(udel, um)
						acl = fmt.Sprintf("%s,%s", um.UIDLocal, acl)
					}
					log.Debugf("[%s] users to be removed: %+v", n, udel)

					// run `setacl -x` command
					if acl != "" {
						cmd = fmt.Sprintf("setfacl -x %s %s", acl, coll.Path)
						if err := repo.IcommandFileOut(cmd, ""); err != nil {
							log.Errorf("[%s] fail setting acl: %s", n, err)
						} else {
							// update uloc with users being removed from access.
							for _, u := range udel {
								delete(uloc, u.UIDRepo)
							}
						}
					}

					// update vdb with the new coll
					coll.ViewersRepo = []string{}
					for k := range uloc {
						coll.ViewersRepo = append(coll.ViewersRepo, k)
					}
					log.Debugf("[%s] collExport: %+v", n, coll)
					if err := vdb.set("cmap", n, &coll); err != nil {
						log.Errorf("[%s] cannot update viewerdb: %s", err)
					}
				}
			}()
		}

		// list all collections within the specified OUs, and with name matching
		// the given pattern.
		for _, ou := range strings.Split(exportOUs, ",") {
			glob := strings.Join([]string{RepoRootPath, ou, exportCollPat}, "/")
			colls, err := filepath.Glob(glob)
			if err != nil {
				log.Errorf("%s", err)
				continue
			}
			for _, c := range colls {
				chanCollExport <- CollExport{
					Path: c,
					OU:   ou,
				}
				log.Debugf("%s", c)
			}
		}
		close(chanCollExport)

		// wait for wg1 to finish
		wg1.Wait()

		return nil
	},
}

func findUmap(pdb pdb.PDB, vdb store, uidRepo string) (Umap, error) {

	um := Umap{}

	log.Debugf("uidRepo: %s", uidRepo)

	// umap found in db, return it directly.
	if err := vdb.get("umap", uidRepo, &um); err == nil {
		return um, nil
	}

	// try to get email of repo user.
	um.UIDRepo = uidRepo
	cmd := fmt.Sprintf(`iquest "%%s" "select META_USER_ATTR_VALUE where USER_NAME = '%s' and META_USER_ATTR_NAME = 'email'"`, uidRepo)
	cout := make(chan string)
	go repo.IcommandChanOut(cmd, &cout, true)
	for l := range cout {
		um.Email = l
	}

	// try to get user's local uid via search email.
	u, err := pdb.GetUserByEmail(um.Email)
	if err != nil {
		return um, err
	}

	// check if the user id resolved from PDB is actually a valid system user id.
	// sometimes the userid in PDB is not consistent with the actual system userid.
	if _, err := user.Lookup(u.ID); err != nil {
		log.Warnf("invalid local user: %s", u.ID)
		return um, err
	}

	// write the new umap into vdb so that it can be reused.
	// only log the error on failure so that the program can continue.
	um.UIDLocal = u.ID
	if err := vdb.set("umap", uidRepo, &um); err != nil {
		log.Errorf("cannot save new umap: %s", err)
	}

	return um, nil
}

// store provides interface to interact with a local database for
// managing/bookkeeping user map and exported collections.
type store struct {
	path  string
	mutex sync.Mutex
	db    *bolt.DB
}

// connect establishes the bolt db connection.
func (s *store) connect() (err error) {
	if s.db != nil {
		return nil
	}

	if s.db, err = bolt.Open(s.path, 0600, nil); err != nil {
		return fmt.Errorf("cannot connect blot db: %s", err)
	}
	return nil
}

// disconnect closes the bold db connection.
func (s *store) disconnect() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// init initialize two buckets in bolt db.
// - umap: for storing uidRepo and uidLocal mapping via email.
// - cmap: for storing a list of locally exported collections and
//         their repo users (in uidRepo) with local access.
func (s *store) init() error {

	if s.db == nil {
		return fmt.Errorf("no connected db")
	}

	// initialize buckets if they don't exist
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for _, bucket := range []string{"umap", "cmap"} {
		if err := s.db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return fmt.Errorf("create bucket: %s", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// get returns a value of the given key within the given bucket
// in the bolt database.
func (s *store) get(bucket, key string, obj interface{}) error {

	if s.db == nil {
		return fmt.Errorf("no connected db")
	}

	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("key %s not in bucket %s", key, bucket)
		}
		if err := json.Unmarshal(v, obj); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

// getAll
func (s *store) getAll(bucket string, objs map[string]interface{}) error {

	if s.db == nil {
		return fmt.Errorf("no connected db")
	}

	if err := s.db.View(func(tx *bolt.Tx) error {
		// Assume bucket exists and has keys
		b := tx.Bucket([]byte(bucket))
		c := b.Cursor()

		for k, v := c.First(); k != nil; k, v = c.Next() {

			switch bucket {
			case "umap":
				objs[string(k)] = &Umap{}
			case "cmap":
				objs[string(k)] = &CollExport{}
			default:
				objs[string(k)] = nil
			}

			if err := json.Unmarshal(v, objs[string(k)]); err != nil {
				log.Errorf("invalid value for %s: %s", k, err)
				continue
			}
		}

		return nil
	}); err != nil {
		// since there is an error, return an empty umap slice.
		return err
	}

	return nil
}

// set
func (s *store) set(bucket, key string, obj interface{}) error {

	if s.db == nil {
		return fmt.Errorf("no connected db")
	}

	// initialize buckets if they don't exist
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		v, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(key), v); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

// // setAllUmap sets all given uidRepo to uidLocal maps in the bolt database.
// func (s *store) setAllUmap(map[string]umap) error {
// 	if s.db == nil {
// 		return fmt.Errorf("no connected db")
// 	}

// 	// initialize buckets if they don't exist
// 	s.mutex.Lock()
// 	defer s.mutex.Unlock()

// }
