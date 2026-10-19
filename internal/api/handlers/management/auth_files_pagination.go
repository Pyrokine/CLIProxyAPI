package management

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

const defaultAuthFilesPageSize = 50

type authFilesPagination struct {
	enabled  bool
	page     int
	pageSize int
}

type diskAuthFileCandidate struct {
	entry os.DirEntry
	info  os.FileInfo
}

func parseAuthFilesPagination(c *gin.Context) (authFilesPagination, error) {
	pageRaw, hasPage := c.GetQuery("page")
	pageSizeRaw, hasPageSize := c.GetQuery("page_size")
	if !hasPage && !hasPageSize {
		return authFilesPagination{}, nil
	}
	pagination := authFilesPagination{enabled: true, page: 1, pageSize: defaultAuthFilesPageSize}
	if hasPage {
		page, err := strconv.Atoi(strings.TrimSpace(pageRaw))
		if err != nil || page <= 0 {
			return authFilesPagination{}, errors.New("page must be a positive integer")
		}
		pagination.page = page
	}
	if hasPageSize {
		pageSize, err := strconv.Atoi(strings.TrimSpace(pageSizeRaw))
		if err != nil || pageSize <= 0 {
			return authFilesPagination{}, errors.New("page_size must be a positive integer")
		}
		pagination.pageSize = pageSize
	}
	return pagination, nil
}

func (p authFilesPagination) bounds(total int) (int, int) {
	if !p.enabled || total <= 0 {
		return 0, total
	}
	if p.page > 1 && p.page-1 > total/p.pageSize {
		return total, total
	}
	start := (p.page - 1) * p.pageSize
	if start >= total {
		return total, total
	}
	remaining := total - start
	if p.pageSize >= remaining {
		return start, total
	}
	return start, start + p.pageSize
}

func authFilesListResponse(files []gin.H, pagination authFilesPagination, total, end int) gin.H {
	response := gin.H{"files": files}
	if pagination.enabled {
		response["total"] = total
		response["page"] = pagination.page
		response["page_size"] = pagination.pageSize
		response["has_more"] = end < total
	}
	return response
}

func authFileListName(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return name
	}
	return strings.TrimSpace(auth.ID)
}

func compareAuthFileListOrder(left, right *coreauth.Auth) int {
	leftName, rightName := authFileListName(left), authFileListName(right)
	if cmp := strings.Compare(strings.ToLower(leftName), strings.ToLower(rightName)); cmp != 0 {
		return cmp
	}
	if cmp := strings.Compare(leftName, rightName); cmp != 0 {
		return cmp
	}
	if cmp := strings.Compare(strings.TrimSpace(left.ID), strings.TrimSpace(right.ID)); cmp != 0 {
		return cmp
	}
	return strings.Compare(strings.TrimSpace(left.Index), strings.TrimSpace(right.Index))
}

func isAuthFileListable(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	runtimeOnly := isRuntimeOnlyAuth(auth)
	if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return false
	}
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" {
		return runtimeOnly
	}
	if _, err := os.Stat(path); os.IsNotExist(err) && !runtimeOnly &&
		(auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api")) {
		return false
	}
	return true
}

func sortedPaginatedAuths(auths []*coreauth.Auth, name, authIndex string, pagination authFilesPagination) ([]*coreauth.Auth, int, int) {
	matching := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if matchesAuthFileLookup(auth, name, authIndex) && isAuthFileListable(auth) {
			auth.EnsureIndex()
			matching = append(matching, auth)
		}
	}
	sort.Slice(matching, func(i, j int) bool {
		return compareAuthFileListOrder(matching[i], matching[j]) < 0
	})
	start, end := pagination.bounds(len(matching))
	return matching[start:end], len(matching), end
}
