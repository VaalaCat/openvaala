package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/spf13/cobra"
)

// newReindexCommand 创建 reindex 命令：按各对话的 metadata.jsonl 重建 metadata.db。
func newReindexCommand() *cobra.Command {
	// 1. 注册 reindex 命令。
	return &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild metadata.db from the conversation directories",
		Long: "Rebuild ~/.local/share/openvaala/metadata.db from every conversation's metadata.jsonl.\n" +
			"Run it after copying conversation directories in, while no other openvaala process uses the data directory.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runReindexCommand(command.Context(), command.OutOrStdout(), command.ErrOrStderr())
		},
	}
}

// runReindexCommand 锁定数据目录，重建元数据索引并输出处理结果。
func runReindexCommand(ctx context.Context, stdout, stderr io.Writer) (err error) {
	// 1. 创建数据目录并取得独占锁；serve 或另一个 reindex 占着锁时返回错误。
	dataDirectory, err := ensureDataDirectory()
	if err != nil {
		return err
	}
	unlock, err := lockDataDirectory(dataDirectory)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, unlock())
	}()
	// 2. 打开存储，返回时关闭。
	opt, closeStorage, err := openStorage(dataDirectory)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, closeStorage())
	}()
	// 3. 重建索引。
	result, err := dao.RebuildMetadataIndex(ctx, opt)
	if err != nil {
		return fmt.Errorf("rebuild metadata index: %w", err)
	}
	// 4. 跳过的对话写到 stderr，已删除的对话和重建数量写到 stdout。
	for _, skipped := range result.SkippedConversations {
		fmt.Fprintf(stderr, "skipped conversation %s: %v\n", skipped.ConversationID, skipped.Reason)
	}
	for _, deleted := range result.DeletedConversations {
		fmt.Fprintf(stdout, "deleted conversation %s\n", deleted)
	}
	fmt.Fprintf(stdout, "rebuilt %d conversations\n", result.RebuiltConversations)
	return nil
}
